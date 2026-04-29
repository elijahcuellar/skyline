package installer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	yaml "github.com/goccy/go-yaml"
)

// Centralized default orders
var (
	defaultTopLevelOrder   = []string{"files", "dnf", "flatpak", "bash"}
	defaultDNFOrder        = []string{"remove", "assert", "install"}
	defaultDNFInstallOrder = []string{"repositories", "gpg_keys", "copr", "packages"}
	defaultBashOrder       = []string{"exec", "scripts"}
)

// Config types matching schema
type Config struct {
	DNF            *DNFSection   `yaml:"dnf,omitempty"`
	Flatpak        *Flatpak      `yaml:"flatpak,omitempty"`
	Files          []FileMapping `yaml:"files,omitempty"`
	Bash           *BashSection  `yaml:"bash,omitempty"`
	ExecutionOrder []string      `yaml:"execution_order"`
}

type FileMapping struct {
	Source      string `yaml:"source"`
	Destination string `yaml:"destination"`
}

type DNFSection struct {
	ExecutionOrder []string      `yaml:"execution_order"`
	Remove         *PackagesList `yaml:"remove,omitempty"`
	Assert         *PackagesList `yaml:"assert,omitempty"`
	Install        *DNFInstall   `yaml:"install,omitempty"`
}

type PackagesList struct {
	Packages []string `yaml:"packages"`
}

type DNFInstall struct {
	ExecutionOrder []string `yaml:"execution_order"`
	Repositories   []string `yaml:"repositories,omitempty"`
	GPGKeys        []string `yaml:"gpg_keys,omitempty"`
	Copr           []string `yaml:"copr,omitempty"`
	Packages       []string `yaml:"packages,omitempty"`
}

type Flatpak struct {
	Repositories []string `yaml:"repositories,omitempty"`
	Apps         []string `yaml:"apps,omitempty"`
}

type BashSection struct {
	ExecutionOrder []string `yaml:"execution_order"`
	Exec           []string `yaml:"exec,omitempty"`
	Scripts        []string `yaml:"scripts,omitempty"`
}

// Installer represents an installer configured from YAML.
type Installer struct {
	cfg  *Config
	home string
	Out  io.Writer
	// Confirm optionally asks the user to confirm an action.
	// Return (true, nil) to proceed, (false, nil) to skip.
	Confirm func(message string) (bool, error)
}

// stepID is a simple internal step identifier type used while running steps.
type stepID string

var stepCounter atomic.Uint64

func newStepID() stepID {
	n := stepCounter.Add(1)
	return stepID(fmt.Sprintf("step-%d", n))
}

// NewFromBytes loads YAML bytes into an Installer.
func NewFromBytes(bts []byte) (*Installer, error) {
	var cfg Config
	if err := yaml.Unmarshal(bts, &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	home, _ := os.UserHomeDir()

	inst := &Installer{
		cfg:  &cfg,
		home: home,
		Out:  os.Stdout,
		// Default Confirm proceeds; callers may override.
		Confirm: func(message string) (bool, error) { return true, nil },
	}

	return inst, nil
}

// NewFromFile loads YAML into an Installer.
func NewFromFile(path string) (*Installer, error) {
	bts, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return NewFromBytes(bts)
}

// Apply runs configured top-level sections.
func (i *Installer) Apply(ctx context.Context) error {
	order := i.cfg.ExecutionOrder
	if len(order) == 0 {
		order = defaultTopLevelOrder
	}
	for _, section := range order {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		switch section {
		case "files":
			if err := i.applyFiles(ctx); err != nil {
				return err
			}
		case "dnf":
			if i.cfg.DNF != nil {
				if err := i.applyDNF(ctx); err != nil {
					return err
				}
			}
		case "flatpak":
			if i.cfg.Flatpak != nil {
				if err := i.applyFlatpak(ctx); err != nil {
					return err
				}
			}
		case "bash":
			if i.cfg.Bash != nil {
				if err := i.applyBash(ctx); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unknown top-level section: %s", section)
		}
	}
	return nil
}

/* ---------- Files ---------- */

func (i *Installer) applyFiles(ctx context.Context) error {
	if len(i.cfg.Files) == 0 {
		return nil
	}
	// lightweight textual indication
	i.Infof("Files: copying")
	for _, f := range i.cfg.Files {
		src := f.Source
		dst := expandHome(f.Destination, i.home)
		op := fmt.Sprintf("copy %s → %s", src, dst)
		if err := i.runWithStep(ctx, op, func(sid stepID) error {
			return i.copySourceToDestination(ctx, sid, src, dst)
		}); err != nil {
			return fmt.Errorf("copy %s: %w", src, err)
		}
	}
	return nil
}

func (i *Installer) copySourceToDestination(ctx context.Context, _ stepID, source, dest string) error {
	// Determine if source is URL or local
	var reader io.ReadCloser
	src := strings.TrimSpace(source)
	var total int64 = -1
	if isURL(src) {
		req, err := http.NewRequestWithContext(ctx, "GET", src, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			if cerr := resp.Body.Close(); cerr != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to close response body: %v\n", cerr)
			}
			return fmt.Errorf("fetch failed: %s", resp.Status)
		}
		reader = resp.Body
		if resp.ContentLength > 0 {
			total = resp.ContentLength
		}
	} else {
		srcPath := expandHome(src, i.home)
		f, err := os.Open(srcPath)
		if err != nil {
			return fmt.Errorf("open source: %w", err)
		}
		reader = f
		if fi, err := f.Stat(); err == nil {
			total = fi.Size()
		}
	}
	defer func() {
		if cerr := reader.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close reader: %v\n", cerr)
		}
	}()

	dest = expandHome(dest, i.home)
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}

	tmp := dest + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create tmp file: %w", err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil {
			// best-effort: log to stderr if closing the temporary file fails
			_, _ = fmt.Fprintf(os.Stderr, "warning: failed to close tmp file: %v\n", cerr)
		}
	}()

	// Copy in a loop.
	buf := make([]byte, 32*1024)
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			wn, werr := out.Write(buf[:n])
			if werr != nil {
				return fmt.Errorf("write file: %w", werr)
			}
			if wn != n {
				return fmt.Errorf("short write")
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return fmt.Errorf("read source: %w", rerr)
		}
	}

	if err := out.Sync(); err != nil {
		// best-effort: report sync failures to stderr to avoid an empty branch.
		_, _ = fmt.Fprintf(os.Stderr, "warning: sync failed: %v\n", err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("rename tmp: %w", err)
	}
	_ = total // keep variable referenced if needed later
	return nil
}

/* ---------- DNF ---------- */

func (i *Installer) applyDNF(ctx context.Context) error {
	i.Infof("DNF: processing")
	order := i.cfg.DNF.ExecutionOrder
	if len(order) == 0 {
		order = defaultDNFOrder
	}
	for _, phase := range order {
		switch phase {
		case "remove":
			if i.cfg.DNF.Remove != nil {
				if err := i.dnfRemove(ctx, i.cfg.DNF.Remove); err != nil {
					return err
				}
			}
		case "assert":
			if i.cfg.DNF.Assert != nil {
				if err := i.dnfAssert(ctx, i.cfg.DNF.Assert); err != nil {
					return err
				}
			}
		case "install":
			if i.cfg.DNF.Install != nil {
				if err := i.dnfInstall(ctx, i.cfg.DNF.Install); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unknown dnf phase: %s", phase)
		}
	}
	return nil
}

func (i *Installer) dnfRemove(ctx context.Context, p *PackagesList) error {
	if p == nil || len(p.Packages) == 0 {
		return nil
	}
	// Batch all package removes into a single dnf/dnf5 invocation to reduce
	// overhead of spawning many subprocesses and repeated metadata checks.
	pkgs := p.Packages
	op := fmt.Sprintf("dnf remove %s", strings.Join(pkgs, " "))
	cmds := []*exec.Cmd{
		buildCmdWithSudo(ctx, "dnf", append([]string{"remove", "-y"}, pkgs...)...),
		buildCmdWithSudo(ctx, "dnf5", append([]string{"remove", "-y"}, pkgs...)...),
	}
	if err := i.tryCommandVariants(ctx, op, cmds); err != nil {
		return err
	}
	return nil
}

func (i *Installer) dnfAssert(ctx context.Context, p *PackagesList) error {
	if p == nil || len(p.Packages) == 0 {
		return nil
	}
	// Check which packages are missing and batch-install them in one operation.
	var missing []string
	for _, pkg := range p.Packages {
		pkgSan := pkg
		checkCmd := exec.CommandContext(ctx, "rpm", "-q", pkgSan)
		if err := i.runCommandWithOutput(ctx, fmt.Sprintf("rpm -q %s", pkgSan), checkCmd); err != nil {
			missing = append(missing, pkgSan)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// Ask for confirmation once for the whole batch.
	if i.Confirm != nil {
		ok, err := i.Confirm("Install DNF packages?")
		if err != nil {
			return fmt.Errorf("confirmation failed: %w", err)
		}
		if !ok {
			if _, ferr := fmt.Fprintln(i.Out, "DNF: assert packages step skipped by user"); ferr != nil {
				_, _ = fmt.Fprintf(os.Stderr, "warning: failed to write to output: %v\n", ferr)
			}
			return nil
		}
	}
	op := fmt.Sprintf("dnf install %s", strings.Join(missing, " "))
	cmds := []*exec.Cmd{
		buildCmdWithSudo(ctx, "dnf", append([]string{"install", "-y"}, missing...)...),
		buildCmdWithSudo(ctx, "dnf5", append([]string{"install", "-y"}, missing...)...),
	}
	if err := i.tryCommandVariants(ctx, op, cmds); err != nil {
		return fmt.Errorf("install failed: %v", err)
	}
	return nil
}

func (i *Installer) dnfInstall(ctx context.Context, ins *DNFInstall) error {
	order := ins.ExecutionOrder
	if len(order) == 0 {
		order = defaultDNFInstallOrder
	}
	for _, step := range order {
		switch step {
		case "repositories":
			for _, repo := range ins.Repositories {
				op := fmt.Sprintf("dnf add-repo %s", repo)
				cmds := []*exec.Cmd{
					buildCmdWithSudo(ctx, "dnf", "config-manager", "addrepo", "--from-repofile="+repo),
					buildCmdWithSudo(ctx, "dnf5", "config-manager", "addrepo", "--from-repofile="+repo),
				}
				if err := i.tryCommandVariants(ctx, op, cmds); err != nil {
					return fmt.Errorf("repository addition failed: %w", err)
				}
			}
		case "gpg_keys":
			for _, key := range ins.GPGKeys {
				op := fmt.Sprintf("rpm import %s", key)
				cmd := buildCmdWithSudo(ctx, "rpm", "--import", key)
				if err := i.runCommandWithOutput(ctx, op, cmd); err != nil {
					return err
				}
			}
		case "copr":
			// Ensure the copr plugin is available; if not, install dnf-plugins-core and retry.
			if err := i.ensureCoprPlugin(ctx); err != nil {
				return fmt.Errorf("copr plugin not available and auto-install failed: %w", err)
			}
			for _, c := range ins.Copr {
				op := fmt.Sprintf("dnf copr enable %s", c)
				cmds := []*exec.Cmd{
					buildCmdWithSudo(ctx, "dnf", "copr", "enable", "-y", c),
					buildCmdWithSudo(ctx, "dnf5", "copr", "enable", "-y", c),
				}
				if err := i.tryCommandVariants(ctx, op, cmds); err != nil {
					return err
				}
			}
		case "packages":
			// Ask for confirmation before installing packages.
			if i.Confirm != nil {
				ok, err := i.Confirm("Install DNF packages?")
				if err != nil {
					return fmt.Errorf("confirmation failed: %w", err)
				}
				if !ok {
					if _, ferr := fmt.Fprintln(i.Out, "DNF: packages step skipped by user"); ferr != nil {
						_, _ = fmt.Fprintf(os.Stderr, "warning: failed to write to output: %v\n", ferr)
					}
					continue
				}
			}
			if len(ins.Packages) == 0 {
				continue
			}
			// Batch install all packages together to speed up operations.
			pkgs := ins.Packages
			op := fmt.Sprintf("dnf install %s", strings.Join(pkgs, " "))
			cmds := []*exec.Cmd{
				buildCmdWithSudo(ctx, "dnf", append([]string{"install", "-y"}, pkgs...)...),
				buildCmdWithSudo(ctx, "dnf5", append([]string{"install", "-y"}, pkgs...)...),
			}
			if err := i.tryCommandVariants(ctx, op, cmds); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown dnf install step: %s", step)
		}
	}
	return nil
}

/* ---------- DNF helpers ---------- */

func (i *Installer) tryCommandVariants(ctx context.Context, op string, cmds []*exec.Cmd) error {
	var errs []string
	for _, c := range cmds {
		if c == nil {
			continue
		}
		if err := i.runCommandWithOutput(ctx, op, c); err == nil {
			return nil
		} else {
			// try next and record error
			ag := ""
			if len(c.Args) > 0 {
				ag = c.Args[0]
			}
			errs = append(errs, fmt.Sprintf("%s: %v", ag, err))
		}
	}
	if len(errs) == 0 {
		return fmt.Errorf("no command variants provided")
	}
	return fmt.Errorf("all variants failed: %s", strings.Join(errs, "; "))
}

/* ---------- Flatpak ---------- */

func (i *Installer) applyFlatpak(ctx context.Context) error {
	// Skip Flatpak if binary not in PATH.
	if !isBinAvailable("flatpak") {
		i.Infof("Flatpak: skipped (flatpak not found)")
		return nil
	}

	i.Infof("Flatpak: processing")
	fp := i.cfg.Flatpak
	if fp == nil {
		return nil
	}
	for _, repo := range fp.Repositories {
		op := fmt.Sprintf("flatpak remote-add %s", repo)
		name := flatpakNameFromURL(repo)
		cmd := exec.CommandContext(ctx, "flatpak", "remote-add", "--if-not-exists", name, repo)
		if err := i.runCommandWithOutput(ctx, op, cmd); err != nil {
			return err
		}
	}
	for _, app := range fp.Apps {
		op := fmt.Sprintf("flatpak install %s", app)
		cmd := exec.CommandContext(ctx, "flatpak", "install", "-y", app)
		if err := i.runCommandWithOutput(ctx, op, cmd); err != nil {
			return err
		}
	}
	return nil
}

func flatpakNameFromURL(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.ReplaceAll(u, "/", "-")
	u = strings.ReplaceAll(u, ":", "")
	if len(u) > 32 {
		u = u[:32]
	}
	return u
}

/* ---------- Bash ---------- */

func (i *Installer) applyBash(ctx context.Context) error {
	i.Infof("Bash: processing")
	order := i.cfg.Bash.ExecutionOrder
	if len(order) == 0 {
		order = defaultBashOrder
	}
	for _, step := range order {
		switch step {
		case "exec":
			for _, cmdStr := range i.cfg.Bash.Exec {
				op := fmt.Sprintf("bash exec: %s", cmdStr)

				// Skip systemd-related commands if systemd/tools are absent.
				if (strings.Contains(cmdStr, "systemctl") || strings.Contains(cmdStr, "loginctl")) && !systemdRunning() {
					i.Infof("bash exec: %s (skipped: systemd not running)", cmdStr)
					continue
				}
				if strings.Contains(cmdStr, "systemctl") && !isBinAvailable("systemctl") {
					i.Infof("bash exec: %s (skipped: systemctl not found)", cmdStr)
					continue
				}
				if strings.Contains(cmdStr, "loginctl") && !isBinAvailable("loginctl") {
					i.Infof("bash exec: %s (skipped: loginctl not found)", cmdStr)
					continue
				}

				cmd := exec.CommandContext(ctx, "bash", "-lc", cmdStr)
				if err := i.runCommandWithOutput(ctx, op, cmd); err != nil {
					return err
				}
			}
		case "scripts":
			for _, s := range i.cfg.Bash.Scripts {
				op := fmt.Sprintf("bash script: %s", s)
				if isURL(s) {
					// Use a shell pipeline for remote scripts (curl | bash)
					cmd := exec.CommandContext(ctx, "bash", "-lc", fmt.Sprintf("curl -fsSL %s | bash -s --", s))
					if err := i.runCommandWithOutput(ctx, op, cmd); err != nil {
						return err
					}
					continue
				}
				path := expandHome(s, i.home)
				// try to make executable, but ignore error
				_ = os.Chmod(path, 0o755)
				cmd := exec.CommandContext(ctx, "bash", path)
				if err := i.runCommandWithOutput(ctx, op, cmd); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unknown bash step: %s", step)
		}
	}
	return nil
}

/* ---------- Utilities / Execution wrappers ---------- */

func expandHome(p string, home string) string {
	if p == "~" {
		return home
	}
	if after, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(home, after)
	}
	return p
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func isRoot() bool {
	// isRoot returns true when running as root.
	return os.Geteuid() == 0
}

// buildCmdWithSudo constructs an exec.Cmd, prefixing with sudo when not root.
func buildCmdWithSudo(ctx context.Context, prog string, args ...string) *exec.Cmd {
	if isRoot() {
		return exec.CommandContext(ctx, prog, args...)
	}
	all := append([]string{prog}, args...)
	return exec.CommandContext(ctx, "sudo", all...)
}

// Infof writes a formatted informational message to the installer's Out if set.
func (i *Installer) Infof(format string, a ...any) {
	if i.Out != nil {
		_, _ = fmt.Fprintln(i.Out, fmt.Sprintf(format, a...))
	}
}

// ringBuffer stores the last N bytes written to it. It is safe to use from
// a single writer goroutine (we only write from one goroutine when streaming
// output), and Bytes() may be called after writes are complete.
const defaultTailBytes = 32 * 1024

type ringBuffer struct {
	cap int
	b   []byte
}

func newRingBuffer(cap int) *ringBuffer { return &ringBuffer{cap: cap, b: make([]byte, 0, cap)} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	if len(p) >= r.cap {
		// keep only last cap bytes of p
		r.b = append(r.b[:0], p[len(p)-r.cap:]...)
		return len(p), nil
	}
	// append then trim if necessary
	r.b = append(r.b, p...)
	if len(r.b) > r.cap {
		r.b = r.b[len(r.b)-r.cap:]
	}
	return len(p), nil
}

func (r *ringBuffer) Bytes() []byte {
	// return a copy to avoid external mutation
	out := make([]byte, len(r.b))
	copy(out, r.b)
	return out
}

// runCommandWithOutput runs cmd and streams its output to the installer's Out.
// Additionally it captures the last N bytes of output in a bounded buffer and
// appends it to the returned error when the command fails.
func (i *Installer) runCommandWithOutput(ctx context.Context, op string, cmd *exec.Cmd) error {
	return i.runWithStep(ctx, op, func(_ stepID) error {
		// Create pipes for stdout and stderr so we can stream and capture last N bytes.
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return err
		}

		if err := cmd.Start(); err != nil {
			return err
		}

		// Writers: stream to i.Out (or discard) and capture last N bytes in ring buffer.
		rb := newRingBuffer(defaultTailBytes) // keep last 32 KiB
		var outWriter = io.Discard
		if i.Out != nil {
			outWriter = i.Out
		}
		mw := io.MultiWriter(outWriter, rb)

		// Copy stdout and stderr concurrently.
		done := make(chan error, 2)
		go func() {
			_, e := io.Copy(mw, stdout)
			done <- e
		}()
		go func() {
			_, e := io.Copy(mw, stderr)
			done <- e
		}()

		// Wait for copies and the command to complete.
		var copyErr error
		for range 2 {
			if e := <-done; e != nil {
				copyErr = e
			}
		}

		if err := cmd.Wait(); err != nil {
			// include captured tail in error
			tail := rb.Bytes()
			if len(tail) > 0 {
				return fmt.Errorf("%v: last %d bytes: %s", err, len(tail), string(tail))
			}
			return err
		}
		// prefer copyErr if present
		if copyErr != nil {
			return copyErr
		}
		return nil
	})
}

// runWithStep runs fn associated with a high-level step and prints concise
// status lines to the installer's output writer.
func (i *Installer) runWithStep(ctx context.Context, message string, fn func(id stepID) error) error {
	type result struct{ err error }

	stepID := newStepID()
	if i.Out != nil {
		_, _ = fmt.Fprintln(i.Out, message)
	}

	resCh := make(chan result, 1)
	go func() {
		resCh <- result{err: fn(stepID)}
	}()

	var res result
	select {
	case res = <-resCh:
		// finished
	case <-ctx.Done():
		res = result{err: ctx.Err()}
	}

	if res.err == nil {
		if i.Out != nil {
			_, _ = fmt.Fprintf(i.Out, "✓ %s (%s)\n", message, time.Since(time.Now()).Round(time.Second))
		}
	} else {
		if i.Out != nil {
			_, _ = fmt.Fprintf(i.Out, "✗ %s (%s) — %v\n", message, time.Since(time.Now()).Round(time.Second), res.err)
		}
	}
	return res.err
}

func isBinAvailable(name string) bool {
	if _, err := exec.LookPath(name); err == nil {
		return true
	}
	return false
}

// ensureCommandAvailable checks if any of the provided binary names exist.
// If none are available, it attempts to install the provided package list via
// dnf (or dnf5) using the existing tryCommandVariants helper. It returns nil
// when at least one binary becomes available, otherwise an error.
func (i *Installer) ensureCommandAvailable(ctx context.Context, bins []string, packages []string) error {
	// Quick check: if any binary is already in PATH, succeed.
	if slices.ContainsFunc(bins, isBinAvailable) {
		return nil
	}

	// Try benign invocation for each candidate binary (e.g., `--help`). If one
	// responds successfully, assume it's available.
	for _, b := range bins {
		check := exec.CommandContext(ctx, b, "--help")
		if err := check.Run(); err == nil {
			return nil
		}
	}

	// Nothing present — attempt to install candidate packages.
	if len(packages) == 0 {
		return fmt.Errorf("no packages provided to install for %v", bins)
	}
	op := fmt.Sprintf("dnf install %s", strings.Join(packages, " "))
	cmds := []*exec.Cmd{
		buildCmdWithSudo(ctx, "dnf", append([]string{"install", "-y"}, packages...)...),
		buildCmdWithSudo(ctx, "dnf5", append([]string{"install", "-y"}, packages...)...),
	}
	if err := i.tryCommandVariants(ctx, op, cmds); err != nil {
		return fmt.Errorf("install %s failed: %w", strings.Join(packages, " "), err)
	}

	// Re-check availability.
	if slices.ContainsFunc(bins, isBinAvailable) {
		return nil
	}
	return fmt.Errorf("none of %v available after installing %s", bins, strings.Join(packages, " "))
}

// ensureCoprPlugin ensures the 'copr' subcommand is available. It will attempt
// to install the 'dnf-plugins-core' package if necessary. It also ensures
// 'mokutil' is present by attempting to install the 'mokutil' package when
// absent.
func (i *Installer) ensureCoprPlugin(ctx context.Context) error {
	// Ensure COPR plugin is available (dnf-plugins-core may provide it).
	if err := i.ensureCommandAvailable(ctx, []string{"copr", "dnf-copr", "dnf-copr-plugin"}, []string{"dnf-plugins-core"}); err != nil {
		return err
	}

	// Ensure curl is available since we use it for remote scripts and downloads.
	if err := i.ensureCommandAvailable(ctx, []string{"curl"}, []string{"curl"}); err != nil {
		return fmt.Errorf("curl check/install failed: %w", err)
	}

	// Also ensure mokutil exists; install if missing. If mokutil installation
	// is not critical for the caller, they can choose to ignore or log the error.
	if err := i.ensureCommandAvailable(ctx, []string{"mokutil"}, []string{"mokutil"}); err != nil {
		return fmt.Errorf("mokutil check/install failed: %w", err)
	}
	return nil
}

func systemdRunning() bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	return true
}
