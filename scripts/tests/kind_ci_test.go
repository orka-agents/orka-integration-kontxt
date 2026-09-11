package scripts_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type commandCall struct {
	Name       string
	Args       []string
	Kubeconfig string
	Revision   string
}

// PATH entries point to this test binary. Git is restricted to the local fixture;
// every command that could contact Docker or Kubernetes is handled here.
func TestMain(m *testing.M) {
	if dir := os.Getenv("KONTXT_RUNNER_TEST_DIR"); dir != "" {
		os.Exit(mockCommand(dir))
	}
	os.Exit(m.Run())
}

func mockCommand(dir string) int {
	c := commandCall{filepath.Base(os.Args[0]), os.Args[1:], os.Getenv("KUBECONFIG"), ""}
	args := strings.Join(c.Args, " ")
	if c.Name == "docker" && strings.HasPrefix(args, "build ") {
		data, _ := os.ReadFile(filepath.Join(c.Args[len(c.Args)-1], "revision"))
		c.Revision = string(data)
	}
	data, _ := json.Marshal(c)
	f, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 90
	}
	_, err = f.Write(append(data, '\n'))
	f.Close()
	if err != nil {
		return 90
	}
	failure := os.Getenv("KONTXT_RUNNER_FAIL")
	switch c.Name {
	case "git":
		if len(c.Args) > 0 && c.Args[0] == "clone" && c.Args[len(c.Args)-2] != os.Getenv("KONTXT_RUNNER_SOURCE") {
			fmt.Fprintln(os.Stderr, "unexpected Git clone source")
			return 91
		}
		cmd := exec.Command(os.Getenv("KONTXT_RUNNER_REAL_GIT"), c.Args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return 1
		}
	case "docker":
		return 0
	case "kind":
		switch {
		case args == "get clusters":
			fmt.Print(os.Getenv("KONTXT_RUNNER_CLUSTERS"))
		case strings.HasPrefix(args, "get nodes "):
			fmt.Println("existing-control-plane")
		case strings.HasPrefix(args, "create cluster "):
			if failure == "create" {
				return 1
			}
			if err := os.WriteFile(option(c.Args, "--kubeconfig"), []byte("scoped fixture\n"), 0600); err != nil {
				return 1
			}
		case strings.HasPrefix(args, "delete cluster "):
		default:
			return 92
		}
	case "runner-registry":
		if c.Args[0] == "push" {
			if failure == "registry" {
				return 1
			}
			fmt.Printf("registry.invalid/%s@sha256:%s\n", c.Args[2], strings.Repeat("a", 64))
		}
	case "openssl":
		if c.Args[0] == "req" {
			for _, flag := range []string{"-keyout", "-out"} {
				if err := os.WriteFile(option(c.Args, flag), []byte("fixture\n"), 0600); err != nil {
					return 1
				}
			}
		} else {
			fmt.Println("fixture")
		}
	case "jq":
		switch {
		case slices.Contains(c.Args, "-n"):
			fmt.Println("{}")
		case strings.Contains(args, ".issuer"):
			io.Copy(io.Discard, os.Stdin)
			fmt.Println("https://kubernetes.default.svc")
		case strings.Contains(args, "Complete"):
			if failure == "job" {
				return 1
			}
		case strings.Contains(args, "Failed"):
			if failure != "job" {
				return 1
			}
		default:
			return 92
		}
	case "kubectl":
		switch {
		case args == "config current-context":
			fmt.Println(os.Getenv("KONTXT_RUNNER_CONTEXT"))
		case strings.Contains(args, "rollout status ") && failure == "rollout":
			return 1
		case strings.Contains(args, "get --raw "):
			fmt.Println(`{"issuer":"https://kubernetes.default.svc"}`)
		case strings.Contains(args, "get job "):
			fmt.Println(`{"status":{}}`)
		case strings.Contains(args, "logs job/"):
			fmt.Println("smoke fixture completed")
		case strings.Contains(args, "-f -"):
			io.Copy(io.Discard, os.Stdin)
		}
	case "helm":
		if failure == "helm" {
			return 1
		}
		for i, arg := range c.Args {
			if arg == "--values" || arg == "-f" {
				if content, err := os.ReadFile(c.Args[i+1]); err != nil || len(content) == 0 {
					return 1
				}
			}
		}
	default:
		return 92
	}
	return 0
}

type runner struct {
	t           *testing.T
	root, dir   string
	source, sha string
	env         map[string]string
	git         string
}

func newRunner(t *testing.T) *runner {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	r := &runner{t: t, root: root, dir: t.TempDir(), git: realGit}
	r.source = filepath.Join(r.dir, "source repository")
	r.env = map[string]string{
		"PATH": filepath.Join(r.dir, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME": os.Getenv("HOME"), "TMPDIR": r.dir,
		"GORACE":              "atexit_sleep_ms=0",
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_TERMINAL_PROMPT": "0",
		"KONTXT_RUNNER_TEST_DIR": r.dir, "KONTXT_RUNNER_REAL_GIT": realGit, "KONTXT_RUNNER_SOURCE": r.source,
		"ORKA_REPOSITORY": r.source, "ORKA_REF": "candidate", "KIND_NODE_IMAGE": "kindest/node:fixture",
		"KUBECONFIG": filepath.Join(r.dir, "inherited-kubeconfig"),
	}
	writeFile(t, r.env["KUBECONFIG"], "inherited config must stay untouched\n")
	writeFile(t, filepath.Join(r.source, "manifest_staging/charts/orka/Chart.yaml"), "apiVersion: v2\nname: orka\nversion: 0.0.1\n")
	writeFile(t, filepath.Join(r.source, "charts/orka/Chart.yaml"), "legacy chart\n")
	writeFile(t, filepath.Join(r.source, "revision"), "main")
	writeFile(t, filepath.Join(r.source, "scripts/lib/kind-local-registry.sh"), `orka_kind_registry_start() {
  export ORKA_KIND_REGISTRY_NAME="registry-$1"
  runner-registry start "$@"
}
orka_kind_registry_push() { runner-registry push "$@"; }
orka_kind_registry_stop() { runner-registry stop "$@"; }
`)
	r.gitRun("init", "--quiet", "-b", "main")
	r.gitRun("add", ".")
	r.gitRun("-c", "user.name=Runner Test", "-c", "user.email=runner@example.invalid", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "main fixture")
	r.gitRun("checkout", "--quiet", "-b", "candidate")
	writeFile(t, filepath.Join(r.source, "revision"), "candidate")
	r.gitRun("add", ".")
	r.gitRun("-c", "user.name=Runner Test", "-c", "user.email=runner@example.invalid", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "candidate fixture")
	r.sha = strings.TrimSpace(r.gitRun("rev-parse", "HEAD"))
	r.gitRun("checkout", "--quiet", "main")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(r.dir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "docker", "kind", "helm", "kubectl", "openssl", "jq", "curl", "runner-registry"} {
		if err := os.Symlink(executable, filepath.Join(r.dir, "bin", name)); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (r *runner) environment() []string {
	var env []string
	for name, value := range r.env {
		env = append(env, name+"="+value)
	}
	return env
}

func (r *runner) gitRun(args ...string) string {
	r.t.Helper()
	cmd := exec.Command(r.git, append([]string{"-C", r.source}, args...)...)
	cmd.Env = r.environment()
	output, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("fixture git failed: %v\n%s", err, output)
	}
	return string(output)
}

func (r *runner) run(wantSuccess bool) []commandCall {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(r.root, "scripts/kind-ci.sh"))
	cmd.Env, cmd.Dir, cmd.WaitDelay = r.environment(), r.root, time.Second
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		r.t.Fatalf("runner did not finish: %v\n%s", ctx.Err(), output)
	}
	if (err == nil) != wantSuccess {
		r.t.Fatalf("runner success = %t, want %t: %v\n%s", err == nil, wantSuccess, err, output)
	}
	if strings.Contains(string(output), "compatibility passed") != wantSuccess || wantSuccess && !strings.Contains(string(output), "compatibility passed against Orka "+r.sha) {
		r.t.Fatalf("runner reported the wrong outcome or revision:\n%s", output)
	}
	f, err := os.Open(filepath.Join(r.dir, "calls.jsonl"))
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	var calls []commandCall
	decoder := json.NewDecoder(f)
	for {
		var call commandCall
		if err := decoder.Decode(&call); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			r.t.Fatal(err)
		}
		calls = append(calls, call)
	}
	return calls
}

func TestKindCISelectedRevisionAndHelmValues(t *testing.T) {
	for _, ref := range []string{"branch", "commit"} {
		t.Run(ref, func(t *testing.T) {
			r := newRunner(t)
			if ref == "commit" {
				r.env["ORKA_REF"] = r.sha
			}
			calls := r.run(true)
			builds := callsMatching(calls, "docker", "build")
			if len(builds) != 3 || builds[0].Revision != "candidate" || builds[1].Revision != "candidate" {
				t.Fatalf("controller and publisher must build the selected revision: %+v", builds)
			}
			helm := callsMatching(calls, "helm", "install")
			if len(helm) != 1 || !strings.HasSuffix(helm[0].Args[2], "/manifest_staging/charts/orka") {
				t.Fatalf("expected selected staging chart installation: %+v", helm)
			}
			var values []string
			for i, arg := range helm[0].Args {
				if arg == "--values" || arg == "-f" {
					values = append(values, helm[0].Args[i+1])
				}
			}
			if len(values) != 2 || filepath.Base(values[0]) != "runtime-values.json" || values[1] != filepath.Join(r.root, "manifests/orka/transaction-token-values.yaml") {
				t.Fatalf("Helm did not receive runtime and transaction-token values: %v", values)
			}
			assertOwnedCleanup(t, r, calls)
		})
	}
}

func TestKindCIFailuresCannotPass(t *testing.T) {
	for _, failure := range []string{"create", "registry", "rollout", "helm", "job"} {
		t.Run(failure, func(t *testing.T) {
			r := newRunner(t)
			r.env["KONTXT_RUNNER_FAIL"] = failure
			assertOwnedCleanup(t, r, r.run(false))
		})
	}
}

func TestKindCIExistingClusterIsPreserved(t *testing.T) {
	for _, failure := range []string{"", "helm"} {
		t.Run("failure="+failure, func(t *testing.T) {
			r := newRunner(t)
			r.env["KIND_USE_EXISTING"], r.env["KIND_CLUSTER_NAME"] = "1", "existing"
			r.env["KUBECONFIG"] = filepath.Join(r.dir, "scoped-config")
			writeFile(t, r.env["KUBECONFIG"], "scoped fixture\n")
			r.env["KONTXT_RUNNER_CONTEXT"], r.env["KONTXT_RUNNER_FAIL"] = "kind-existing", failure
			calls := r.run(failure == "")
			assertNoOwnedResources(t, calls)
			for _, c := range callsMatching(calls, "kubectl") {
				if c.Kubeconfig != r.env["KUBECONFIG"] {
					t.Fatalf("kubectl did not use the supplied config: %+v", c)
				}
			}
			if data, err := os.ReadFile(r.env["KUBECONFIG"]); err != nil || string(data) != "scoped fixture\n" {
				t.Fatal("runner changed the supplied kubeconfig")
			}
		})
	}
}

func TestKindCIRejectsUnsafeExistingConfig(t *testing.T) {
	for _, scenario := range []string{"missing", "multiple", "mismatched", "global", "global-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			r := newRunner(t)
			r.env["KIND_USE_EXISTING"], r.env["KIND_CLUSTER_NAME"] = "1", "existing"
			r.env["KONTXT_RUNNER_CONTEXT"] = "kind-existing"
			switch scenario {
			case "missing":
				delete(r.env, "KUBECONFIG")
			case "multiple":
				r.env["KUBECONFIG"] += ":another-config"
			case "mismatched":
				r.env["KONTXT_RUNNER_CONTEXT"] = "kind-other"
			case "global", "global-symlink":
				r.env["KUBECONFIG"] = filepath.Join(os.Getenv("HOME"), ".kube/config")
				if scenario == "global-symlink" {
					link := filepath.Join(r.dir, "config-link")
					if err := os.Symlink(r.env["KUBECONFIG"], link); err != nil {
						t.Fatal(err)
					}
					r.env["KUBECONFIG"] = link
				}
			}
			calls := r.run(false)
			assertNoOwnedResources(t, calls)
			for _, c := range calls {
				if c.Name == "kubectl" && !(scenario == "mismatched" && slices.Equal(c.Args, []string{"config", "current-context"})) || c.Name == "helm" || c.Name == "runner-registry" || c.Name == "kind" {
					t.Fatalf("unsafe config reached a cluster operation: %+v", c)
				}
			}
		})
	}
}

func TestKindCIDoesNotReplaceExistingName(t *testing.T) {
	r := newRunner(t)
	r.env["KIND_CLUSTER_NAME"], r.env["KONTXT_RUNNER_CLUSTERS"] = "existing", "existing\n"
	calls := r.run(false)
	assertNoOwnedResources(t, calls)
	if calls := callsMatching(calls, "kubectl"); len(calls) != 0 {
		t.Fatalf("name collision reached Kubernetes: %+v", calls)
	}
}

func assertOwnedCleanup(t *testing.T, r *runner, calls []commandCall) {
	t.Helper()
	created, deleted := callsMatching(calls, "kind", "create", "cluster"), callsMatching(calls, "kind", "delete", "cluster")
	if len(created) != 1 || len(deleted) != 1 || option(created[0].Args, "--name") != option(deleted[0].Args, "--name") {
		t.Fatalf("cleanup did not target only the created cluster: created=%+v deleted=%+v", created, deleted)
	}
	config := option(created[0].Args, "--kubeconfig")
	if config == r.env["KUBECONFIG"] || !strings.HasPrefix(config, r.dir+string(os.PathSeparator)) {
		t.Fatalf("cluster did not receive a private config: %q", config)
	}
	if created[0].Kubeconfig != config || deleted[0].Kubeconfig != config {
		t.Fatal("cluster creation or cleanup accessed another kubeconfig")
	}
	for _, c := range callsMatching(calls, "kubectl") {
		if c.Kubeconfig != config {
			t.Fatalf("kubectl accessed another config: %+v", c)
		}
	}
	started, stopped := callsMatching(calls, "runner-registry", "start"), callsMatching(calls, "runner-registry", "stop")
	if len(started) != len(stopped) || len(started) == 1 && !slices.Equal(started[0].Args[1:], stopped[0].Args[1:]) {
		t.Fatalf("registry cleanup changed ownership: started=%+v stopped=%+v", started, stopped)
	}
	if _, err := os.Stat(filepath.Dir(config)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runner left its temporary directory: %v", err)
	}
	content, err := os.ReadFile(r.env["KUBECONFIG"])
	if err != nil || string(content) != "inherited config must stay untouched\n" {
		t.Fatal("runner changed the inherited kubeconfig")
	}
}

func assertNoOwnedResources(t *testing.T, calls []commandCall) {
	t.Helper()
	for _, pair := range [][2]string{{"kind", "create"}, {"kind", "delete"}, {"runner-registry", "stop"}} {
		if found := callsMatching(calls, pair[0], pair[1]); len(found) != 0 {
			t.Fatalf("runner changed resources it did not own: %+v", found)
		}
	}
}

func callsMatching(calls []commandCall, name string, prefix ...string) []commandCall {
	var found []commandCall
	for _, c := range calls {
		if c.Name == name && len(c.Args) >= len(prefix) && slices.Equal(c.Args[:len(prefix)], prefix) {
			found = append(found, c)
		}
	}
	return found
}

func option(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
