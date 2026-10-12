package sqlite

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// ValidateRestoreStageRecoveryState never applies or repairs whole-DB restore.
// A retained candidate is ambiguous even when its marker looks terminal.
func ValidateRestoreStageRecoveryState(path string) error {
	reject := func() error { return errors.New("whole database restore must be resolved before stage recovery") }
	state, err := RestoreStatus(path)
	if err != nil {
		return reject()
	}
	raw, err := os.ReadFile(path + ".restore.json")
	if os.IsNotExist(err) {
		if state.Status != "none" {
			return reject()
		}
	} else {
		if err != nil || len(raw) > 64<<10 {
			return reject()
		}
		var strict RestoreState
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&strict) != nil || d.Decode(new(any)) != io.EOF {
			return reject()
		}
		if strict != state || state.Name == "" || filepath.Base(state.Name) != state.Name || state.UpdatedAt.IsZero() {
			return reject()
		}
		if state.Status != "failed" && state.Status != "applied" {
			return reject()
		}
		if state.Status == "applied" && state.LastError != "" || state.Status == "failed" && state.LastError == "" {
			return reject()
		}
	}
	if _, err := os.Lstat(path + ".restore"); !os.IsNotExist(err) {
		return reject()
	}
	if _, err := os.Lstat(path + ".restore-original"); err == nil {
		if state.Status == "none" || ValidateSnapshot(path+".restore-original") != nil {
			return reject()
		}
	} else if !os.IsNotExist(err) {
		return reject()
	}
	return nil
}
