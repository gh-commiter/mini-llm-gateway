package main

// Container mode (docs/docker.md): the gateway in the supported Docker Compose deployment. The image sets
// MINIGW_CONTAINER=1; nothing is guessed from /.dockerenv or cgroups. In this mode the gateway is providers only:
// Claude CLI, Codex, Grok, Copilot and Foundry are off and stay off (a sandbox for them is not available in the default
// container). The rule is applied at start, at every settings change and at every read of the settings, so an imported
// or hand-edited serve.json cannot turn an agent on.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	containerConfig  = "/var/lib/minigw/config/serve.json" // the config volume
	containerScratch = "/var/lib/minigw/scratch"           // the Scratch volume
	containerRefusal = "this gateway runs in a container (Docker Compose) and serves providers only: Claude CLI, Codex, Grok, Copilot and Foundry Local are not available there"
)

func containerMode() bool { return os.Getenv("MINIGW_CONTAINER") == "1" }

// containerAgentsOn: names of the local agents the config turns on ("" list = none).
func containerAgentsOn(c serveConfig) []string {
	var on []string
	for _, a := range []struct {
		name string
		on   bool
	}{{"claude", !c.ClaudeDisabled}, {"codex", c.Codex.Enabled}, {"grok", c.Grok.Enabled}, {"copilot", c.Copilot.Enabled}, {"foundry", c.Foundry.Enabled}} {
		if a.on {
			on = append(on, a.name)
		}
	}
	return on
}

// containerForce turns every local agent off (container mode only); reports whether it changed anything.
func containerForce(c *serveConfig) bool {
	if !containerMode() || len(containerAgentsOn(*c)) == 0 {
		return false
	}
	c.ClaudeDisabled, c.Codex.Enabled, c.Grok.Enabled, c.Copilot.Enabled, c.Foundry.Enabled = true, false, false, false, false
	return true
}

// containerCheck: the error for a change that would turn an agent on (nil outside container mode).
func containerCheck(c serveConfig) error {
	if !containerMode() {
		return nil
	}
	if on := containerAgentsOn(c); len(on) > 0 {
		return fmt.Errorf("%s (%v)", containerRefusal, on)
	}
	return nil
}

// containerInit: first start. Creates a missing config (absolute Scratch in the Scratch volume, agents off), keeps an
// existing one, prints no token: the owner reads the connect line with `docker compose exec minigw minigw gw token`.
// Idempotent; returns whether it created the config.
func containerInit(cfgPath string, port int) (bool, error) {
	abs, _ := filepath.Abs(filepath.Dir(cfgPath))
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return false, err
	}
	if err := protectConfigDir(abs); err != nil {
		return false, fmt.Errorf("%s: %v: the config volume must belong to the gateway user (uid 10001); see docs/docker.md", abs, err)
	}
	defer lockServeConfig(cfgPath)()
	c, err := loadServeConfig(cfgPath)
	created := errors.Is(err, os.ErrNotExist)
	if created {
		c = newServeConfig()
		c.Scratch = containerScratch
	} else if err != nil {
		return false, err
	}
	if port != 0 {
		c.Port = port
	}
	containerForce(&c)
	return created, writeServeConfig(cfgPath, c)
}

// healthcheck: GET /health/live on loopback, bounded; no credentials, no provider calls.
func healthcheck(port int, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:"+strconv.Itoa(port)+"/health/live", nil)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("health/live answered " + resp.Status)
	}
	return nil
}
