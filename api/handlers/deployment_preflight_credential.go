package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/promlifecycle/platform/models"
)

const deploymentPreflightTTL = 10 * time.Minute

type canonicalDeploymentRequest struct {
	Type                 string                 `json:"type"`
	Name                 string                 `json:"name"`
	HostID               uint                   `json:"host_id"`
	Version              string                 `json:"version"`
	Port                 int                    `json:"port"`
	DeployType           string                 `json:"deploy_type"`
	Config               map[string]interface{} `json:"config,omitempty"`
	ResourceLimits       string                 `json:"resource_limits,omitempty"`
	CustomEnv            string                 `json:"custom_env,omitempty"`
	StructuredParameters map[string]interface{} `json:"structured_parameters,omitempty"`
	ExtraVolumes         string                 `json:"extra_volumes,omitempty"`
	DataPathOverride     string                 `json:"data_path_override,omitempty"`
	PlannedComponents    []string               `json:"planned_components,omitempty"`
	PlannedVersions      map[string]string      `json:"planned_versions,omitempty"`
	RequiredComponents   []string               `json:"required_components,omitempty"`
	InstallPath          string                 `json:"install_path,omitempty"`
	CustomArgs           string                 `json:"custom_args,omitempty"`
	InitialPassword      string                 `json:"initial_password,omitempty"`
	ReplaceExisting      bool                   `json:"replace_existing"`
}

func deploymentRequestHash(request componentCreateRequest) (string, error) {
	hostID := uint(0)
	if request.HostID != nil {
		hostID = *request.HostID
	}
	canonical := canonicalDeploymentRequest{
		Type: request.Type, Name: request.Name, HostID: hostID,
		Version: request.Version, Port: request.Port, DeployType: request.DeployType,
		Config: request.Config, ResourceLimits: request.ResourceLimits,
		CustomEnv: request.CustomEnv, ExtraVolumes: request.ExtraVolumes,
		DataPathOverride: request.DataPathOverride, InstallPath: request.InstallPath,
		CustomArgs: request.CustomArgs, InitialPassword: request.InitialPassword,
		StructuredParameters: request.StructuredParameters,
		ReplaceExisting:      request.ReplaceExisting,
		PlannedComponents:    request.PlannedComponents, PlannedVersions: request.PlannedVersions, RequiredComponents: request.RequiredComponents,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func deploymentDefinitionHash(definition models.DeploymentDefinition, custom *models.ComponentDefinition) (string, error) {
	value := interface{}(definition)
	if custom != nil {
		value = struct {
			Definition models.DeploymentDefinition        `json:"definition"`
			Snapshot   models.ComponentDefinitionSnapshot `json:"snapshot"`
		}{Definition: definition, Snapshot: custom.Snapshot()}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (h *Handler) issueDeploymentPreflight(request componentCreateRequest, definition models.DeploymentDefinition, custom *models.ComponentDefinition, checks []deploymentPreflightCheck, actions []deploymentPreviewAction, actor string) (models.DeploymentPreflight, error) {
	requestHash, err := deploymentRequestHash(request)
	if err != nil {
		return models.DeploymentPreflight{}, err
	}
	definitionHash, err := deploymentDefinitionHash(definition, custom)
	if err != nil {
		return models.DeploymentPreflight{}, err
	}
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return models.DeploymentPreflight{}, err
	}
	evidence, err := json.Marshal(map[string]interface{}{
		"checks": checks, "actions": actions,
	})
	if err != nil {
		return models.DeploymentPreflight{}, err
	}
	now := time.Now()
	credential := models.DeploymentPreflight{
		Token: hex.EncodeToString(tokenBytes[:]), RequestHash: requestHash,
		ComponentType: request.Type, DefinitionHash: definitionHash,
		HostID: dereferenceHostID(request.HostID), Port: request.Port,
		CreatedBy: actor, Evidence: string(evidence), ExpiresAt: now.Add(deploymentPreflightTTL), CreatedAt: now,
	}
	if err := h.db.Create(&credential).Error; err != nil {
		return models.DeploymentPreflight{}, err
	}
	// Consumed credentials are useful briefly for audit/debugging; expired and
	// old consumed rows carry no durable product value.
	cutoff := now.Add(-24 * time.Hour)
	_ = h.db.Where("expires_at < ? OR (consumed_at IS NOT NULL AND consumed_at < ?)", now, cutoff).Delete(&models.DeploymentPreflight{}).Error
	return credential, nil
}

func (h *Handler) validateAndConsumeDeploymentPreflight(request componentCreateRequest, definition models.DeploymentDefinition, custom *models.ComponentDefinition, host models.Host) error {
	if request.PreflightToken == "" {
		return fmt.Errorf("缺少部署预检凭证，请重新执行部署预检")
	}
	var credential models.DeploymentPreflight
	if err := h.db.First(&credential, "token = ?", request.PreflightToken).Error; err != nil {
		return fmt.Errorf("部署预检凭证不存在，请重新执行部署预检")
	}
	now := time.Now()
	if credential.ConsumedAt != nil {
		return fmt.Errorf("部署预检凭证已经使用，不能重复部署")
	}
	if !credential.ExpiresAt.After(now) {
		return fmt.Errorf("部署预检凭证已过期，请重新执行部署预检")
	}
	requestHash, err := deploymentRequestHash(request)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(requestHash), []byte(credential.RequestHash)) != 1 {
		return fmt.Errorf("部署参数、目标主机或组件名称在预检后发生变化，请重新执行部署预检")
	}
	definitionHash, err := deploymentDefinitionHash(definition, custom)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(definitionHash), []byte(credential.DefinitionHash)) != 1 {
		return fmt.Errorf("部署定义在预检后发生变化，请重新执行部署预检")
	}
	if credential.ComponentType != request.Type || credential.HostID != host.ID || credential.Port != request.Port {
		return fmt.Errorf("部署预检凭证与当前目标不匹配，请重新执行部署预检")
	}
	if err := validateDeploymentHost(definition, &request, &host); err != nil {
		return err
	}

	// A preview may describe future stack members; execution may depend only
	// on components that actually exist now, never on client promises.
	request.PlannedComponents = nil
	request.PlannedVersions = nil
	checks := make([]deploymentPreflightCheck, 0, 5)
	if request.DeployType == "target" {
		address := net.JoinHostPort(host.IP, strconv.Itoa(request.Port))
		connection, _ := net.DialTimeout("tcp", address, 5*time.Second)
		if connection != nil {
			_ = connection.Close()
		}
	} else {
		h.preflightDependencies(&request, &host, &checks)
		h.preflightPackage(&request, &definition, custom, &checks)
		h.preflightHostAccess(&request, &definition, &host, &checks)
		h.preflightPaths(&request, &definition, &checks)
		h.preflightPort(&request, &host, &checks)
		h.preflightPromtailSources(&request, &host, &checks)
		h.preflightGrafanaCredentials(&request, &host, &checks)
		for _, check := range checks {
			if check.Blocking && check.Status == "fail" {
				return fmt.Errorf("部署前重新检查失败：%s：%s", check.Name, check.Message)
			}
		}
	}
	consumedAt := time.Now()
	result := h.db.Model(&models.DeploymentPreflight{}).
		Where("token = ? AND consumed_at IS NULL AND expires_at > ?", credential.Token, consumedAt).
		Update("consumed_at", &consumedAt)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("部署预检凭证已过期或被其他请求使用")
	}
	return nil
}

func dereferenceHostID(hostID *uint) uint {
	if hostID == nil {
		return 0
	}
	return *hostID
}
