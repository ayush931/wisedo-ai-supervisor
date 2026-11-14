package supervisor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

func TestWIS708OfflineModelStartup(t *testing.T) {
	if out, err := exec.Command("sh", "model-entrypoint-test.sh").CombinedOutput(); err != nil {
		t.Fatalf("offline startup regression: %v\n%s", err, out)
	}
}

// WIS-708 review finding 2: the release bundle omitted the model entrypoint, so the models
// profile could not start from wisedo_deploy/. Every relative file a staged compose file
// mounts, and every compose file or sample the staged README tells the operator to use, must
// be in the bundle.
func TestWIS708StagedBundleHasReferencedFiles(t *testing.T) {
	out := filepath.Join(t.TempDir(), "wisedo_deploy")
	cmd := exec.Command("bash", "../../scripts/stage_deploy_folder.sh")
	cmd.Env = append(os.Environ(), "VERSION=0.0.0-test", "STAGE_DIR="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stage: %v\n%s", err, b)
	}
	dir := filepath.Join(out, "deploy", "supervisor")
	mounts := regexp.MustCompile(`(?m)^\s*(?:-\s*|source:\s*)\./([^\s:]+)`)
	readme := regexp.MustCompile(`deploy/supervisor/([\w.-]+\.(?:yml|png))`)
	want := map[string]bool{}
	for _, name := range []string{"compose.yml", "compose.models-runtime.yml"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("staged %s: %v", name, err)
		}
		for _, m := range mounts.FindAllStringSubmatch(string(b), -1) {
			want[m[1]] = true
		}
	}
	if !want["model-entrypoint.sh"] || !want["bootstrap.sh"] {
		t.Fatalf("mount scan found %v; expected bootstrap.sh and model-entrypoint.sh", want)
	}
	b, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range readme.FindAllStringSubmatch(string(b), -1) {
		want[m[1]] = true
	}
	for name := range want {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || !fi.Mode().IsRegular() {
			t.Errorf("staged bundle lacks %s, which a staged compose file or README references", name)
		}
	}
}

// WIS-708: two concurrent ~12k-token local calls overflowed llm-chat's single 16k slot, opened
// the provider circuit breaker and refused every chat. The rendered runtime overlay must default
// api and worker to one call at a time, matching llm-chat's --parallel slot count.
func TestWIS708LocalModelConcurrencyMatchesChatSlots(t *testing.T) {
	dir := t.TempDir()
	envFile, runtimeEnv := filepath.Join(dir, "compose.env"), filepath.Join(dir, "runtime.env")
	vars := "SUPERVISOR_RUNTIME_ENV_FILE=" + runtimeEnv + "\n"
	for _, v := range []string{"POSTGRES_ADMIN_PASSWORD", "SUPERVISOR_MIGRATION_PASSWORD", "SUPERVISOR_RUNTIME_PASSWORD", "KEYCLOAK_DB_PASSWORD", "TEMPORAL_DB_PASSWORD", "KEYCLOAK_ADMIN_PASSWORD"} {
		vars += v + "=dummy\n"
	}
	if err := os.WriteFile(envFile, []byte(vars), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeEnv, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "compose", "--env-file", envFile, "-f", "compose.yml", "-f", "compose.models-runtime.yml",
		"--profile", "models", "--profile", "runtime", "config", "--format", "json")
	// Only what docker needs: an inherited SUPERVISOR_AGENT_MAX_CONCURRENT must not mask the default.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for _, v := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG"} {
		if val, ok := os.LookupEnv(v); ok {
			cmd.Env = append(cmd.Env, v+"="+val)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		t.Fatalf("docker compose config (docker is required for this test): %v\n%s\n%s", err, out, stderr)
	}
	var cfg struct {
		Services map[string]struct {
			Command     []string          `json:"command"`
			Environment map[string]string `json:"environment"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	slots := ""
	chat := cfg.Services["llm-chat"].Command
	for i, arg := range chat[:max(len(chat)-1, 0)] {
		if arg == "--parallel" {
			slots = chat[i+1]
		}
	}
	if slots != "1" {
		t.Fatalf("llm-chat --parallel = %q, want 1 (the 16k context holds one ~12k call): %v", slots, chat)
	}
	for _, svc := range []string{"api", "worker"} {
		if got := cfg.Services[svc].Environment["SUPERVISOR_AGENT_MAX_CONCURRENT"]; got != slots {
			t.Errorf("%s SUPERVISOR_AGENT_MAX_CONCURRENT = %q, want %s (llm-chat --parallel)", svc, got, slots)
		}
	}
}
