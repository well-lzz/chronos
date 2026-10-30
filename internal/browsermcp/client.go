// Package browsermcp bridges the official Playwright MCP stdio server to the
// Go executor. Each executor gets an isolated browser and a scoped proxy.
package browsermcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const packageVersion = "@playwright/mcp@0.0.83"

// Only the core exploration tools are available to the model. In particular,
// arbitrary JavaScript evaluation and local file operations are excluded.
var allowedTools = map[string]bool{
	"browser_navigate": true, "browser_navigate_back": true,
	"browser_snapshot": true, "browser_find": true,
	"browser_click": true, "browser_hover": true,
	"browser_type": true, "browser_fill_form": true,
	"browser_select_option": true, "browser_press_key": true,
	"browser_console_messages": true, "browser_network_requests": true,
	"browser_network_request": true,
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type Client struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	cancel    context.CancelFunc
	proxy     *Proxy
	outputDir string
	nextID    int
	tools     []Tool
}

// Available checks the local runtime without fetching packages or launching a browser.
func Available() bool {
	node := os.Getenv("CHRONOS_PLAYWRIGHT_NODE")
	if node == "" {
		node = "node"
	}
	output, err := exec.Command(node, "--version").Output()
	if err != nil {
		return false
	}
	major, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(strings.TrimSpace(string(output)), "v"), ".", 2)[0])
	if err != nil || major < 18 {
		return false
	}
	if cli := os.Getenv("CHRONOS_PLAYWRIGHT_CLI"); cli != "" {
		_, err = os.Stat(cli)
		return err == nil
	}
	_, err = exec.LookPath("npx")
	return err == nil
}

func Start(ctx context.Context, allowed func(string) bool) (*Client, error) {
	proxy, err := StartProxy(allowed)
	if err != nil {
		return nil, err
	}
	outputDir, err := os.MkdirTemp("", "chronos-playwright-*")
	if err != nil {
		proxy.Close()
		return nil, err
	}
	processCtx, cancel := context.WithCancel(ctx)
	args := []string{"--headless", "--browser=chromium", "--isolated", "--block-service-workers", "--no-webmcp",
		"--proxy-server=" + proxy.URL(), "--proxy-bypass=<-loopback>", "--output-dir=" + outputDir}
	if os.Getenv("CHRONOS_PLAYWRIGHT_NO_SANDBOX") == "true" {
		args = append(args, "--no-sandbox")
	}
	var cmd *exec.Cmd
	if cli := os.Getenv("CHRONOS_PLAYWRIGHT_CLI"); cli != "" {
		node := os.Getenv("CHRONOS_PLAYWRIGHT_NODE")
		if node == "" {
			node = "node"
		}
		cmd = exec.CommandContext(processCtx, node, append([]string{cli}, args...)...)
	} else {
		cmd = exec.CommandContext(processCtx, "npx", append([]string{"--yes", packageVersion}, args...)...)
	}
	// Do not let inherited Playwright settings disable the scoped proxy or attach
	// to an operator's existing browser.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "PLAYWRIGHT_MCP_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		proxy.Close()
		_ = os.RemoveAll(outputDir)
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		proxy.Close()
		_ = os.RemoveAll(outputDir)
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		proxy.Close()
		_ = os.RemoveAll(outputDir)
		return nil, err
	}
	c := &Client{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), cancel: cancel, proxy: proxy, outputDir: outputDir}
	startup, stop := context.WithTimeout(ctx, 60*time.Second)
	defer stop()
	var initResult json.RawMessage
	if err := c.call(startup, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "chronos", "version": "1"},
	}, &initResult); err != nil {
		c.Close()
		return nil, fmt.Errorf("MCP initialize: %w", err)
	}
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		c.Close()
		return nil, err
	}
	var listed struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(startup, "tools/list", map[string]any{}, &listed); err != nil {
		c.Close()
		return nil, fmt.Errorf("MCP tools/list: %w", err)
	}
	for _, tool := range listed.Tools {
		if allowedTools[tool.Name] {
			c.tools = append(c.tools, tool)
		}
	}
	if len(c.tools) == 0 {
		c.Close()
		return nil, errors.New("playwright MCP returned no browser exploration tools")
	}
	return c, nil
}

func (c *Client) Tools() []Tool { return append([]Tool(nil), c.tools...) }

func (c *Client) notify(method string, params any) error {
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	_, err := c.stdin.Write(append(data, '\n'))
	return err
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if _, err = c.stdin.Write(append(data, '\n')); err != nil {
		return err
	}
	type answer struct {
		result json.RawMessage
		err    error
	}
	ch := make(chan answer, 1)
	go func() {
		for {
			line, err := c.stdout.ReadBytes('\n')
			if err != nil {
				ch <- answer{err: err}
				return
			}
			var envelope struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(line, &envelope) != nil || envelope.ID != id {
				continue
			}
			if envelope.Error != nil {
				ch <- answer{err: errors.New(envelope.Error.Message)}
			} else {
				ch <- answer{result: envelope.Result}
			}
			return
		}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			return got.err
		}
		return json.Unmarshal(got.result, out)
	case <-ctx.Done():
		c.cancel()
		return ctx.Err()
	}
}

func (c *Client) Call(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
	if !allowedTools[name] {
		return "", fmt.Errorf("browser tool %q is unavailable", name)
	}
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, &result); err != nil {
		return "", err
	}
	var parts []string
	for _, item := range result.Content {
		if item.Type == "text" {
			parts = append(parts, item.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if len(text) > 20000 {
		text = text[:20000] + "\n[truncated]"
	}
	if result.IsError {
		return text, errors.New("playwright MCP tool error")
	}
	return text, nil
}

func (c *Client) Close() {
	c.cancel()
	_ = c.stdin.Close()
	_ = c.cmd.Wait()
	c.proxy.Close()
	_ = os.RemoveAll(c.outputDir)
}
