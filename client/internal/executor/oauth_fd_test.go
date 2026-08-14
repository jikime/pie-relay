package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStartPassesOAuthThroughPrivateDescriptorNotEnvironment(t *testing.T) {
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("env command is unavailable")
	}
	directory := t.TempDir()
	script := filepath.Join(directory, "probe.mjs")
	source := `import {closeSync,readFileSync} from 'node:fs';
const fd=Number(process.env.PIE_CLAUDE_OAUTH_FD);
const token=readFileSync(fd,'utf8'); closeSync(fd);
process.stdout.write(JSON.stringify({type:'oauth_probe',privateFD:fd>=3,tokenLength:token.length,nodeEnvHasSecret:Boolean(process.env.CLAUDE_CODE_OAUTH_TOKEN||process.env.ANTHROPIC_API_KEY)})+'\n');`
	if err := os.WriteFile(script, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor, err := StartWithOptions(ctx, "node", script, StartOptions{ClaudeOAuthToken: "sk-ant-oat-private-descriptor-test-000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	event, ok := <-executor.Events()
	if !ok {
		t.Fatal("probe exited without an event")
	}
	var result struct {
		PrivateFD        bool `json:"privateFD"`
		TokenLength      int  `json:"tokenLength"`
		NodeEnvHasSecret bool `json:"nodeEnvHasSecret"`
	}
	if err := json.Unmarshal(event.Raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.PrivateFD || result.TokenLength == 0 || result.NodeEnvHasSecret {
		t.Fatalf("unexpected descriptor probe: %+v", result)
	}
	_ = executor.Close()
}
