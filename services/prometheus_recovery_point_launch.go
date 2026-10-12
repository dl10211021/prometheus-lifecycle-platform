package services

import (
	"context"
	"fmt"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"strconv"
	"strings"
)

// Typed busctl output is supported by systemd 239 and retains argv boundaries.
// This initial subset accepts printable ASCII without escapes, plus literal
// direct ExecStart syntax to reject execution modifiers hidden by legacy D-Bus.
func verifyRecoveryPointLaunch(ctx context.Context, r hostCommandRunner, c *models.Component, argvSHA string) error {
	unit := deployer.CanonicalServiceName(c.ServiceName) + ".service"
	var escaped strings.Builder
	for i, b := range []byte(unit) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || (i > 0 && b >= '0' && b <= '9') {
			escaped.WriteByte(b)
		} else {
			fmt.Fprintf(&escaped, "_%02x", b)
		}
	}
	object := "/org/freedesktop/systemd1/unit/" + escaped.String()
	bus := "busctl get-property org.freedesktop.systemd1 " + utils.QuoteShell(object) + " org.freedesktop.systemd1.Service "
	cmd := "# promlifecycle:recovery-point-launch\n" + bus + "ExecStart | head -c 262145"
	out, err := snapshotRun(ctx, r, "bash -o pipefail -c "+utils.QuoteShell(cmd))
	if err != nil {
		return err
	}
	if _, err := parseRecoveryPointBusLaunch(out, c, argvSHA); err != nil {
		return err
	}
	cmd = "# promlifecycle:recovery-point-namespace\nset -eu\nfor prop in RootDirectory RootImage BindPaths BindReadOnlyPaths TemporaryFileSystem DynamicUser PrivateTmp ProtectHome PAMName InaccessiblePaths; do\n" + bus + "\"$prop\"\ndone"
	out, err = snapshotRun(ctx, r, "bash -o pipefail -c "+utils.QuoteShell("("+cmd+") | head -c 4097"))
	if err != nil {
		return err
	}
	expected := []string{`s ""`, `s ""`, "a(ssbt) 0", "a(ssbt) 0", "a(ss) 0", "b false", "b false", `s "no"`, `s ""`, "as 0"}
	actual := strings.Split(strings.TrimSpace(out), "\n")
	if len(out) > 4096 || len(actual) != len(expected) || (actual[6] != "b false" && actual[6] != "b true") {
		return pointReject("unsupported loaded filesystem or dynamic-user context")
	}
	if actual[6] == "b true" {
		if _, _, err := deployer.PrometheusRuntimePaths(c); err != nil {
			return pointReject("PrivateTmp requires canonical non-private Prometheus runtime paths")
		}
	}
	actual[6] = "b false"
	if !equalStringSlices(actual, expected) {
		return pointReject("unsupported loaded filesystem or dynamic-user context")
	}
	_, files, err := readUnitSnapshotState(ctx, r, unit)
	if err != nil {
		return err
	}
	found := false
	for _, file := range files {
		if file.Role != "fragment" && file.Role != "dropin" {
			continue
		}
		raw, err := snapshotRun(ctx, r, "head -c 1048577 -- "+utils.QuoteShell(file.Path))
		if err != nil {
			return err
		}
		has, err := validateRecoveryPointUnitText(raw, c, argvSHA)
		if err != nil {
			return err
		}
		found = found || has
	}
	if !found {
		return pointReject("direct literal ExecStart source required")
	}
	cmd = "# promlifecycle:recovery-point-hooks\nsystemctl show --all --property=ExecStartPre,ExecStartPost,ExecStop,ExecStopPost,ExecCondition " + utils.QuoteShell(unit) + " | head -c 4097"
	hooks, err := snapshotRun(ctx, r, "bash -o pipefail -c "+utils.QuoteShell(cmd))
	if err != nil {
		return err
	}
	if len(hooks) > 4096 {
		return pointReject("oversized lifecycle hook evidence")
	}
	// Empty Exec arrays are omitted even with --all on supported systemd versions.
	if strings.TrimSpace(hooks) == "" {
		return nil
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(hooks), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || value != "" || seen[key] {
			return pointReject("lifecycle hooks are unsupported")
		}
		switch key {
		case "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "ExecCondition":
		default:
			return pointReject("unknown lifecycle hook evidence")
		}
		seen[key] = true
	}
	return nil
}

type recoveryBusToken struct {
	value  string
	quoted bool
}

func recoveryPointBusTokens(raw string) ([]recoveryBusToken, error) {
	if len(raw) == 0 || len(raw) > 256<<10 {
		return nil, pointReject("invalid typed launch size")
	}
	tokens := []recoveryBusToken{}
	for i := 0; i < len(raw); {
		if raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r' {
			i++
			continue
		}
		quoted := raw[i] == '"'
		if quoted {
			i++
		}
		start := i
		for i < len(raw) {
			ch := raw[i]
			if quoted && ch == '"' || !quoted && (ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r') {
				break
			}
			if ch < 32 || ch > 126 || ch == '\\' || ch == '"' {
				return nil, pointReject("unsupported escaped typed launch")
			}
			i++
		}
		value := raw[start:i]
		if quoted {
			if i == len(raw) {
				return nil, pointReject("unterminated typed launch string")
			}
			i++
			if i < len(raw) && !strings.ContainsRune(" \t\r\n", rune(raw[i])) {
				return nil, pointReject("missing typed token separator")
			}
		}
		tokens = append(tokens, recoveryBusToken{value, quoted})
		if len(tokens) > 1040 {
			return nil, pointReject("too many typed launch fields")
		}
	}
	return tokens, nil
}
func parseRecoveryPointBusLaunch(raw string, c *models.Component, argvSHA string) ([]string, error) {
	tokens, err := recoveryPointBusTokens(raw)
	if err != nil {
		return nil, err
	}
	reject := func() ([]string, error) { return nil, pointReject("invalid or changed typed ExecStart") }
	if len(tokens) < 13 || tokens[0].quoted || tokens[0].value != "a(sasbttttuii)" || tokens[1].quoted || tokens[1].value != "1" || !tokens[2].quoted || tokens[2].value != c.BinaryPath || tokens[3].quoted {
		return reject()
	}
	n, err := strconv.ParseUint(tokens[3].value, 10, 16)
	if err != nil || n == 0 || n > 1024 || len(tokens) != int(n)+12 {
		return reject()
	}
	args := make([]string, n)
	for i := range args {
		token := tokens[4+i]
		if !token.quoted || token.value == "" {
			return reject()
		}
		args[i] = token.value
	}
	index := 4 + len(args)
	if tokens[index].quoted || tokens[index].value != "false" {
		return reject()
	}
	index++
	for i := 0; i < 7; i++ {
		token := tokens[index+i]
		if token.quoted {
			return reject()
		}
		bits := 64
		if i == 4 {
			bits = 32
		}
		if i < 5 {
			_, err = strconv.ParseUint(token.value, 10, bits)
		} else {
			_, err = strconv.ParseInt(token.value, 10, 32)
		}
		if err != nil {
			return reject()
		}
	}
	joined := []byte(strings.Join(args, "\x00") + "\x00")
	if len(joined) > 64<<10 || args[0] != c.BinaryPath || pointDigest(joined) != argvSHA {
		return reject()
	}
	return args, nil
}
func validateRecoveryPointUnitText(raw string, c *models.Component, argvSHA string) (bool, error) {
	if len(raw) > 1<<20 || strings.ContainsRune(raw, 0) {
		return false, pointReject("invalid unit source size")
	}
	service, found := false, false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		// systemd joins physical continuations before discarding comments.
		// Reject them before any comment/section handling to keep this strict
		// subset independent of those parser state transitions.
		if strings.HasSuffix(line, "\\") {
			return false, pointReject("continued unit syntax is unsupported")
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, ".include") {
			return false, pointReject("external unit include is unsupported")
		}
		if strings.HasPrefix(line, "[") {
			switch line {
			case "[Service]":
				service = true
			case "[Unit]", "[Install]":
				service = false
			default:
				return false, pointReject("unsupported unit section")
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// These newer remappings may be unavailable over D-Bus on systemd 239.
		switch key {
		case "MountImages", "ExtensionImages", "ExtensionDirectories", "RootEphemeral", "JoinsNamespaceOf", "RootDirectoryStartOnly":
			if value != "" && value != "no" && value != "false" {
				return false, pointReject("unsupported unit namespace directive")
			}
		}
		if !service || key != "ExecStart" || value == "" {
			continue
		}
		if !strings.HasPrefix(value, c.BinaryPath+" ") {
			return false, pointReject("literal direct ExecStart required")
		}
		remainder := strings.TrimPrefix(value, c.BinaryPath+" ")
		args := []string{c.BinaryPath}
		if strings.HasPrefix(remainder, `"`) {
			for remainder != "" {
				if !strings.HasPrefix(remainder, `"`) {
					return false, pointReject("mixed or ambiguous quoted ExecStart syntax")
				}
				remainder = strings.TrimPrefix(remainder, `"`)
				end := strings.IndexByte(remainder, '"')
				if end <= 0 {
					return false, pointReject("empty or unterminated quoted ExecStart argument")
				}
				arg := remainder[:end]
				if strings.ContainsAny(arg, "\t\r\n\\'`$%") {
					return false, pointReject("quoted ExecStart escapes or expansion are unsupported")
				}
				for _, ch := range []byte(arg) {
					if ch < 32 || ch > 126 {
						return false, pointReject("only literal ASCII ExecStart is supported")
					}
				}
				args = append(args, arg)
				remainder = remainder[end+1:]
				if remainder == "" {
					break
				}
				if !strings.HasPrefix(remainder, ` "`) {
					return false, pointReject("ambiguous quoted ExecStart spacing")
				}
				remainder = strings.TrimPrefix(remainder, " ")
			}
		} else {
			if strings.ContainsAny(value, "\t\r\\'\"`$%") {
				return false, pointReject("literal direct ExecStart required")
			}
			for _, ch := range []byte(value) {
				if ch < 32 || ch > 126 {
					return false, pointReject("only literal ASCII ExecStart is supported")
				}
			}
			args = strings.Split(value, " ")
			for _, arg := range args {
				if arg == "" {
					return false, pointReject("ambiguous direct ExecStart spacing")
				}
			}
		}
		if pointDigest([]byte(strings.Join(args, "\x00")+"\x00")) != argvSHA {
			return false, pointReject("unit source launch differs from observed runtime")
		}
		found = true
	}
	return found, nil
}
