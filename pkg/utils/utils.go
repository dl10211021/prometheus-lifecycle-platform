package utils

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ParseUintParam parses and validates a uint parameter from gin context
func ParseUintParam(c *gin.Context, name string) (uint, bool) {
	val := c.Param(name)
	id, err := strconv.ParseUint(val, 10, 32)
	if err != nil || val == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid %s: %s", name, val)})
		return 0, false
	}
	return uint(id), true
}

// HandleGORMError converts GORM errors to HTTP responses
func HandleGORMError(c *gin.Context, err error) {
	if err == nil {
		return
	}

	// Record not found
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "resource not found"})
		return
	}

	// Unique constraint violation
	if strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "unique constraint") {
		c.JSON(http.StatusConflict, gin.H{"error": "duplicate resource"})
		return
	}

	// Invalid query (e.g., missing WHERE conditions)
	if strings.Contains(err.Error(), "WHERE conditions required") {
		c.JSON(http.StatusNotFound, gin.H{"error": "resource not found"})
		return
	}

	// Generic error
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
}

// HandleNotFoundError returns a consistent not found error
func HandleNotFoundError(c *gin.Context, resource string) {
	c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("%s not found", resource)})
}

// HandleValidationError returns a validation error
func HandleValidationError(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": message})
}

// HandleConflictError returns a conflict error
func HandleConflictError(c *gin.Context, message string) {
	c.JSON(http.StatusConflict, gin.H{"error": message})
}

// SanitizeInput sanitizes a string input
func SanitizeInput(s string) string {
	// Remove null bytes
	s = strings.ReplaceAll(s, "\x00", "")
	// Strip HTML tags for XSS prevention
	var result strings.Builder
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			continue
		}
		if !inTag {
			result.WriteRune(r)
		}
	}
	s = result.String()
	// Trim whitespace
	s = strings.TrimSpace(s)
	// Limit length
	if len(s) > 1000 {
		s = s[:1000]
	}
	return s
}

// SafePathSegment reports whether s can be used as a single path segment:
// no separators, no traversal, no NUL bytes, and within a sane length.
// It is meant for values that later become part of filesystem paths
// (cluster names, component names, config names, cert names).
func SafePathSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if len(s) > 128 {
		return false
	}
	return !strings.ContainsAny(s, "/\\\x00")
}

// PathWithinBase reports whether path is lexically contained by base. Callers
// handling existing files must additionally reject symbolic links with Lstat.
func PathWithinBase(base, path string) bool {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(baseAbs, pathAbs)
	if err != nil || rel == "." {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ValidEnvironmentName reports whether name is a portable environment
// variable identifier. Restricting names also prevents generated systemd unit
// directives from being injected through custom environment keys.
func ValidEnvironmentName(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if i == 0 {
			if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && c != '_' {
				return false
			}
			continue
		}
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') &&
			(c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// QuoteShell single-quotes a value for use in remote/local shell commands so
// it is treated as one literal argument regardless of content.
func QuoteShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// SafeInstallPath validates an executable installation directory. Empty means
// "use the component default". Platform-owned subdirectories under /opt,
// /usr/local, /srv and other non-system roots are allowed; OS-owned trees are
// rejected because uninstall may recursively remove this directory.
func SafeInstallPath(p string) bool {
	if p == "" {
		return true
	}
	if !safeManagedPathSyntax(p) {
		return false
	}
	lower := strings.ToLower(p)
	if managedRootOnly(lower) {
		return false
	}
	for _, root := range []string{"/etc", "/bin", "/sbin", "/boot", "/dev", "/proc", "/sys", "/var", "/root", "/home", "/lib", "/lib64", "/run"} {
		if pathAtOrBelow(lower, root) {
			return false
		}
	}
	if pathAtOrBelow(lower, "/usr") && !strings.HasPrefix(lower, "/usr/local/") {
		return false
	}
	return lower != "/usr/local"
}

// SafeDataPath validates a persistent data directory. /var/lib/<component> is
// intentionally allowed; system roots and unrelated /var subtrees are not.
func SafeDataPath(p string) bool {
	if p == "" {
		return true
	}
	if !safeManagedPathSyntax(p) {
		return false
	}
	lower := strings.ToLower(p)
	if managedRootOnly(lower) {
		return false
	}
	for _, root := range []string{"/etc", "/usr", "/bin", "/sbin", "/boot", "/dev", "/proc", "/sys", "/root", "/home", "/lib", "/lib64", "/run"} {
		if pathAtOrBelow(lower, root) {
			return false
		}
	}
	if pathAtOrBelow(lower, "/var") {
		return strings.HasPrefix(lower, "/var/lib/") && lower != "/var/lib/"
	}
	return true
}

func safeManagedPathSyntax(p string) bool {
	if len(p) > 512 || !strings.HasPrefix(p, "/") || p == "/" {
		return false
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." || segment == "." {
			return false
		}
	}
	return true
}

func pathAtOrBelow(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

func managedRootOnly(path string) bool {
	for _, root := range []string{"/opt", "/srv", "/data", "/mnt", "/tmp", "/app", "/u01"} {
		if path == root {
			return true
		}
	}
	return false
}

// IsValidComponentType checks if a component type is valid
func IsValidComponentType(t string) bool {
	validTypes := map[string]bool{
		"prometheus":        true,
		"grafana":           true,
		"alertmanager":      true,
		"loki":              true,
		"promtail":          true,
		"thanos":            true,
		"node_exporter":     true,
		"windows_exporter":  true,
		"pushgateway":       true,
		"blackbox_exporter": true,
	}
	return validTypes[t]
}

// PathTraversalCheck checks for path traversal attempts
func PathTraversalCheck(path string) bool {
	return strings.Contains(path, "..") ||
		strings.Contains(path, "//") ||
		strings.Contains(path, "\x00")
}

// IsUniqueConstraintError checks if an error is a unique constraint violation
func IsUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint") ||
		strings.Contains(err.Error(), "unique constraint")
}

// SanitizeHTML - alias for SanitizeInput
func SanitizeHTML(s string) string {
	return SanitizeInput(s)
}

// IsDuplicateEntryError - alias for IsUniqueConstraintError
func IsDuplicateEntryError(err error) bool {
	return IsUniqueConstraintError(err)
}

// AtomicWriteFile writes data to path via a temp file + rename so concurrent
// writers (scheduler sync loops and request handlers) can never interleave
// into a half-written config file, and readers never observe partial content.
func AtomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// JSONBytesToMap parses a JSON object into a string map, coercing non-string
// values via fmt. Empty input yields an empty map without error.
func JSONBytesToMap(data []byte, out *map[string]string) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		var raw map[string]interface{}
		if err2 := json.Unmarshal(data, &raw); err2 != nil {
			return fmt.Errorf("failed to parse JSON: %w", err2)
		}
		for k, v := range raw {
			if str, ok := v.(string); ok {
				(*out)[k] = str
			} else if v != nil {
				(*out)[k] = fmt.Sprintf("%v", v)
			}
		}
	}
	return nil
}

// Build metadata is accepted only on a complete SemVer identity. Keep the
// legacy charset rule below for versions without metadata; component-specific
// compatibility checks still decide whether a prerelease/version is supported.
var versionWithBuildMetadata = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*$`)

// ValidVersion reports whether s is a safe component version string. Versions
// become part of filesystem paths (binary cache keys) and are user input.
// Legacy versions allow an optional "v", digit start, and alphanumerics with
// . - _ separators; complete SemVer versions may also have +build metadata.
func ValidVersion(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	if strings.ContainsRune(s, '+') {
		return versionWithBuildMetadata.MatchString(s)
	}
	i := 0
	if s[0] == 'v' {
		i = 1
	}
	if i >= len(s) || s[i] < '0' || s[i] > '9' {
		return false
	}
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c == '.' || c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

// ValidIPv4 reports whether s is a dotted-quad IPv4 address with each octet
// in 0-255. IPs become connection targets and filter keys, so format alone
// is not enough (999.999.999.999 passes a naive regex).
func ValidIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}
