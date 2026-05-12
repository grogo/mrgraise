package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/ini.v1"
)

var phrasesToFilter = []string{
	"EXAM:", "EXAM DATE:", "TECHNIQUE:", "COMPARISON:", "IV Contrast:",
	"Oral Contrast:", "RADIATION DOSE:", "If you are a provider",
	"RADIOPHARMACEUTICAL:", "FINGERSTICK", "UPTAKE TIME:", "LIMITATIONS:",
	"Reference", "Comment:",
}

// Config holds the application configuration loaded from ini files.
type Config struct {
	Model       string
	Temperature float64
	MaxTokens   int
	APIKey      string
	// Prompts
	SystemPrompt       string
	CheckReportQuery   string
	GenImpressionQuery string
}

// APIMessage represents a single message in the API request.
type APIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// APIRequest is the request body for the Anthropic messages API.
type APIRequest struct {
	Model       string       `json:"model"`
	MaxTokens   int          `json:"max_tokens"`
	Temperature float64      `json:"temperature"`
	System      string       `json:"system"`
	Messages    []APIMessage `json:"messages"`
}

// ContentBlock represents a content block in the API response.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// APIResponse is the response body from the Anthropic messages API.
type APIResponse struct {
	Content []ContentBlock `json:"content"`
}

// APIError represents an error response from the Anthropic API.
type APIError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func loadConfig() Config {
	exeDir := getExeDir()

	// Defaults for the model in case .ini file can't be read
	cfg := Config{
		Model:       "claude-haiku-4-5-20251001",
		Temperature: 0.3,
		MaxTokens:   4096,
	}

	// Load defaults.ini
	defaultsPath := filepath.Join(exeDir, "defaults.ini")
	if iniFile, err := ini.Load(defaultsPath); err == nil {
		if key, err := iniFile.Section("model").GetKey("name"); err == nil {
			cfg.Model = key.String()
		}
		if key, err := iniFile.Section("api").GetKey("temperature"); err == nil {
			if v, err := key.Float64(); err == nil {
				cfg.Temperature = v
			}
		}
		if key, err := iniFile.Section("api").GetKey("max_tokens"); err == nil {
			if v, err := key.Int(); err == nil {
				cfg.MaxTokens = v
			}
		}
	}

	// Load API key from secrets.ini
	secretsPath := filepath.Join(exeDir, "secrets.ini")
	if iniFile, err := ini.Load(secretsPath); err == nil {
		if key, err := iniFile.Section("api").GetKey("key"); err == nil {
			cfg.APIKey = key.String()
		}
	}

	// Load prompts from prompts.ini
	promptsPath := filepath.Join(exeDir, "prompts.ini")
	promptsFile, err := ini.Load(promptsPath)
	assert(err == nil, fmt.Sprintf("Failed to load prompts.ini: %v", err))
	promptsSec := promptsFile.Section("prompts")
	cfg.SystemPrompt = promptsSec.Key("system").String()
	cfg.CheckReportQuery = promptsSec.Key("check_report").String()
	cfg.GenImpressionQuery = promptsSec.Key("gen_impression").String()
	assert(cfg.SystemPrompt != "", "system prompt not found in prompts.ini")
	assert(cfg.CheckReportQuery != "", "check_report prompt not found in prompts.ini")
	assert(cfg.GenImpressionQuery != "", "gen_impression prompt not found in prompts.ini")

	assert(cfg.APIKey != "", "API key not found. Create a secrets.ini file with [api] key = your-api-key")
	return cfg
}

func getExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		// Fall back to working directory
		dir, _ := os.Getwd()
		return dir
	}
	return filepath.Dir(exe)
}

// LLM config is loaded the first time an LLM hotkey is invoked so users
// who don't have prompts.ini / secrets.ini set up can still use the
// non-LLM hotkeys without the program panicking at startup.
var (
	llmCfgOnce sync.Once
	llmCfg     Config
	llmCfgErr  error
)

func getLLMConfig() (Config, error) {
	llmCfgOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				llmCfgErr = fmt.Errorf("%v", r)
			}
		}()
		llmCfg = loadConfig()
	})
	return llmCfg, llmCfgErr
}

func queryClaude(cfg Config, userPrompt string, userInput string) (string, error) {
	assert(userPrompt != "", "userPrompt must not be empty")
	assert(userInput != "", "userInput must not be empty")

	reqBody := APIRequest{
		Model:       cfg.Model,
		MaxTokens:   cfg.MaxTokens,
		Temperature: cfg.Temperature,
		System:      cfg.SystemPrompt,
		Messages: []APIMessage{
			{Role: "user", Content: userPrompt},
			{Role: "user", Content: userInput},
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr APIError
		if json.Unmarshal(body, &apiErr) == nil {
			return "", fmt.Errorf("API error (%d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		return "", fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
	}

	var apiResp APIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(apiResp.Content) == 0 {
		return "", fmt.Errorf("empty response from API")
	}

	return strings.TrimSpace(apiResp.Content[0].Text), nil
}

// processReportText sends userInput to Claude with either the
// generate-impression or check-report prompt and prints the response.
// In impression mode the cleaned impression text is also placed on the
// clipboard so the user can paste it back into the report with Ctrl+V.
func processReportText(cfg Config, userInput string, generateImpression bool) {
	filtered := filterParagraphs(userInput, phrasesToFilter)

	if generateImpression {
		result, err := queryClaude(cfg, cfg.GenImpressionQuery, filtered)
		if err != nil {
			showError(fmt.Sprintf("LLM error: %v", err))
			return
		}
		errs := extractErrorsBeforeImpression(result)
		paragraphs := numberParagraphs(stripMarkdown(removeNumbering(result)))
		fmt.Printf("=============================\n")

		fmt.Printf("ERRORS:\n %s\n\nIMPRESSION:\n%s\n\n", errs, paragraphs)
		if err := setClipboardText(paragraphs); err != nil {
			showError("Clipboard error: " + err.Error())
		}
		return
	}

	result, err := queryClaude(cfg, cfg.CheckReportQuery, filtered)
	if err != nil {
		showError(fmt.Sprintf("LLM error: %v", err))
		return
	}
	fmt.Printf("=============================\n")
	fmt.Printf("%s\n\n", stripMarkdown(result))
}
