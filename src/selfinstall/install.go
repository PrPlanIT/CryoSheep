package selfinstall

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Paths and units an installation owns. Variables so tests can point them at a temp dir.
var (
	BinaryPath  = "/usr/local/bin/cryosheep"
	UnitDir     = "/etc/systemd/system"
	ChannelFile = "/etc/cryosheep/channel"
	// Enabled are the units an install arms; cryosheep@.service is started by NUT only.
	Enabled = []string{"cryosheep-stasis.service", "cryosheep-wake.service"}
	// Systemctl runs systemctl; replaced in tests.
	Systemctl = func(args ...string) error {
		out, err := exec.Command("systemctl", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
)

// Report lists what an operation changed, for the operator.
type Report []string

func (r *Report) add(format string, a ...any) { *r = append(*r, fmt.Sprintf(format, a...)) }

// Install writes the binary and units, records the channel, and arms the units.
// Running it again converges: unchanged files are left alone.
func Install(binary []byte, units fs.FS, ch Channel, enable bool) (Report, error) {
	var rep Report
	if err := writeAtomic(BinaryPath, binary, 0o755, &rep); err != nil {
		return rep, err
	}
	changed, err := writeUnits(units, &rep)
	if err != nil {
		return rep, err
	}
	if err := writeAtomic(ChannelFile, []byte(string(ch)+"\n"), 0o644, &rep); err != nil {
		return rep, err
	}
	if changed {
		if err := Systemctl("daemon-reload"); err != nil {
			return rep, err
		}
		rep.add("systemd reloaded")
	}
	if enable {
		args := append([]string{"enable", "--now"}, Enabled...)
		if err := Systemctl(args...); err != nil {
			return rep, err
		}
		rep.add("enabled %s", strings.Join(Enabled, ", "))
	}
	return rep, nil
}

// InstalledChannel returns the channel recorded at install, defaulting to stable.
func InstalledChannel() Channel {
	b, err := os.ReadFile(ChannelFile)
	if err != nil {
		return Stable
	}
	if Channel(strings.TrimSpace(string(b))) == Dev {
		return Dev
	}
	return Stable
}

// Remove disarms and deletes the units, the binary and the channel record. The run
// journal and the NUT configuration are not CryoSheep's to remove.
func Remove() (Report, error) {
	var rep Report
	args := append([]string{"disable", "--now"}, Enabled...)
	if err := Systemctl(args...); err != nil {
		return rep, err
	}
	rep.add("disabled %s", strings.Join(Enabled, ", "))
	units, _ := filepath.Glob(filepath.Join(UnitDir, "cryosheep*.service"))
	for _, p := range append(units, BinaryPath, ChannelFile) {
		if err := os.Remove(p); err == nil {
			rep.add("removed %s", p)
		} else if !os.IsNotExist(err) {
			return rep, err
		}
	}
	if err := Systemctl("daemon-reload"); err != nil {
		return rep, err
	}
	rep.add("systemd reloaded")
	return rep, nil
}

func writeUnits(units fs.FS, rep *Report) (bool, error) {
	entries, err := fs.Glob(units, "*.service")
	if err != nil {
		return false, err
	}
	changed := false
	for _, name := range entries {
		b, err := fs.ReadFile(units, name)
		if err != nil {
			return false, err
		}
		before := len(*rep)
		if err := writeAtomic(filepath.Join(UnitDir, name), b, 0o644, rep); err != nil {
			return false, err
		}
		changed = changed || len(*rep) > before
	}
	return changed, nil
}

// writeAtomic replaces path with content via a rename, and only when it differs.
func writeAtomic(path string, content []byte, mode os.FileMode, rep *Report) error {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, content) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".new")
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	rep.add("wrote %s", path)
	return nil
}
