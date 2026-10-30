package solver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/browsermcp"
)

func (s *Solver) ensureBrowser(ctx context.Context) error {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.browser != nil {
		return nil
	}
	client, err := browsermcp.Start(ctx, func(host string) bool { return s.scopeSnapshot().Allows(host) })
	if err != nil {
		return err
	}
	launch, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err = client.Call(launch, "browser_snapshot", json.RawMessage("{}")); err != nil {
		client.Close()
		return fmt.Errorf("launch browser: %w", err)
	}
	s.browser = client
	s.rec("tool_ready", "playwright_mcp", "Playwright MCP browser started")
	return nil
}

// CloseBrowser releases the MCP process and its proxy at executor handoff or exit.
func (s *Solver) CloseBrowser() {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.browser != nil {
		s.browser.Close()
		s.browser = nil
	}
	s.browserURL = ""
}

func (s *Solver) browserTools() []openai.Tool {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.browser == nil {
		return nil
	}
	var out []openai.Tool
	for _, tool := range s.browser.Tools() {
		if tool.InputSchema == nil {
			continue
		}
		out = append(out, openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema,
		}})
	}
	return out
}

func (s *Solver) toolBrowser(ctx context.Context, name, argsJSON string) string {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.browser == nil {
		return "Playwright MCP browser is unavailable"
	}
	navigateURL := ""
	if name == "browser_navigate" {
		var a struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "browser_navigate error: bad arguments"
		}
		if err := browsermcp.CheckURL(a.URL, func(host string) bool { return s.scopeSnapshot().Allows(host) }); err != nil {
			return "【平台管控·非目标防御】BLOCKED (scope lock): " + err.Error()
		}
		navigateURL = a.URL
	}
	if browserMutation(name) {
		if s.browserURL == "" {
			return "browser interaction blocked: navigate to an in-scope URL first"
		}
		surface, _, err := canonicalSurface(s.browserURL)
		if err != nil {
			return "browser interaction blocked: " + err.Error()
		}
		if blocked := s.requireMutationLease(surface); blocked != "" {
			return blocked
		}
	}
	result, err := s.browser.Call(ctx, name, json.RawMessage(argsJSON))
	if err != nil {
		return fmt.Sprintf("%s error: %v %s", name, err, result)
	}
	if navigateURL != "" {
		s.browserURL = navigateURL
	}
	for _, line := range strings.Split(result, "\n") {
		if raw, ok := strings.CutPrefix(strings.TrimSpace(line), "- Page URL: "); ok {
			if browsermcp.CheckURL(raw, func(host string) bool { return s.scopeSnapshot().Allows(host) }) == nil {
				s.browserURL = raw
			} else {
				s.browserURL = ""
			}
			break
		}
	}
	if browserPageAction(name) {
		if snapshot, snapshotErr := s.browser.Call(ctx, "browser_snapshot", json.RawMessage(`{}`)); snapshotErr == nil {
			result += "\n\n### Current accessibility snapshot\n" + snapshot
		}
	}
	return result
}

func browserMutation(name string) bool {
	switch name {
	case "browser_click", "browser_type", "browser_fill_form", "browser_select_option", "browser_press_key":
		return true
	default:
		return false
	}
}

func isBrowserTool(name string) bool { return strings.HasPrefix(name, "browser_") }

func browserPageAction(name string) bool {
	return name == "browser_navigate" || name == "browser_navigate_back" || browserMutation(name)
}
