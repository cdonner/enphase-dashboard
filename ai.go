package main

// AI suggestions for unnamed appliance signatures. The prompt can be copied into any
// chat app; optionally the Claude API (ANTHROPIC_API_KEY) or Gemini API (GEMINI_API_KEY,
// gemini.go) answers directly. Only summary numbers per signature and the household
// description the user wrote are sent; no raw readings or location.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

const (
	// Claude Haiku 5.5 keeps a request at a fraction of a cent; AI_MODEL overrides it
	// (e.g. claude-opus-5-5 for the most capable model).
	defaultAIModel   = "claude-haiku-5-5"
	aiFile           = "ai-suggestions.json"
	aiMaxClusters    = 15
	aiRequestTimeout = 5 * time.Minute
)

type AIGuess struct {
	Name       string `json:"name"`
	Confidence string `json:"confidence"` // high, medium, low
	Reason     string `json:"reason"`
}

type AISuggestion struct {
	At      int64     `json:"at"`
	Source  string    `json:"source,omitempty"` // provider that answered
	Model   string    `json:"model,omitempty"`  // model that answered
	Guesses []AIGuess `json:"guesses"`
}

// aiEnabled reports whether the SDK will find credentials: an API key, an auth token, or
// workload identity federation (which the SDK detects and exchanges by itself).
func aiEnabled() bool {
	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		return true
	}
	for _, k := range []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_SERVICE_ACCOUNT_ID"} {
		if os.Getenv(k) == "" {
			return false
		}
	}
	return os.Getenv("ANTHROPIC_IDENTITY_TOKEN_FILE") != "" || os.Getenv("ANTHROPIC_IDENTITY_TOKEN") != ""
}

const aiSystem = `You help identify household electrical appliances from whole-house power data
(non-intrusive load monitoring). For each signature you get: the step size in watts, which
circuit it is on (240 V across both legs, 120 V on one leg, or 240 V plus extra on one leg -
e.g. a heating element plus a motor), whether it is resistive (heating element) or motor/
inductive from the power factor, how long it typically stays on, how regularly it repeats,
and at which hours of the day it starts. The household description from the owner tells you
which appliances exist; prefer those, and do not suggest appliances the owner says are off,
absent or on gas, unless the evidence is strong (then say so in the reason).

Loads that switch on and off while nobody is using anything are usually equipment that runs on
its own: furnace or air handler blowers (often several speeds, each a separate signature), inducer
fans and igniters, refrigerator and freezer compressors and their defrost or ice-maker heaters,
dehumidifiers, sump or well pumps, water treatment, aquarium or terrarium heaters, heated seats,
and electronics waking from standby. A signature that starts together with an identified
appliance is very likely part of it (e.g. another stage or motor of the same furnace). If nothing
plausible fits, answer with a single guess named "Unknown" and low confidence rather than inventing
an appliance the owner doesn't have.

Give up to 3 guesses per signature, best first. Use plain appliance names a homeowner would
use (e.g. "Coffee maker warming plate", "Upstairs furnace blower"). Confidence "high" only
when power, circuit, character and timing all fit one appliance. Keep each reason to one
short sentence citing the decisive evidence.`

var aiSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"signatures": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key": map[string]any{"type": "string"},
					"guesses": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name":       map[string]any{"type": "string"},
								"confidence": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
								"reason":     map[string]any{"type": "string"},
							},
							"required":             []string{"name", "confidence", "reason"},
							"additionalProperties": false,
						},
					},
				},
				"required":             []string{"key", "guesses"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"signatures"},
	"additionalProperties": false,
}

func fmtSeconds(s float64) string {
	switch {
	case s < 90:
		return fmt.Sprintf("%.0f s", s)
	case s < 5400:
		return fmt.Sprintf("%.0f min", s/60)
	default:
		return fmt.Sprintf("%.1f h", s/3600)
	}
}

// describeForAI renders one cluster as compact text for the prompt.
func describeForAI(c Cluster, legs int) string {
	var hours []string
	for h, n := range c.Hours {
		if n > 0 {
			hours = append(hours, fmt.Sprintf("%02d:%d", h, n))
		}
	}
	gap := "unknown"
	if c.MedianGapSec > 0 {
		gap = "every " + fmtSeconds(c.MedianGapSec)
	}
	with := ""
	for _, w := range c.With {
		with += fmt.Sprintf("; starts within %s of %q in %d of its %d runs", coStartWindow, w.Name, w.Runs, c.Runs)
	}
	return fmt.Sprintf("- key %s: %.0f W step, %s, %s (reactive/real %.2f); %d runs on %d of the last %d days; "+
		"on for %s typically (range %s-%s); starts %s; energy %s in the period; start counts by hour (hh:count) %s%s",
		c.Key, c.Signature.PowerW, c.Signature.Legs, c.Signature.Character, reactiveRatio(c.Signature),
		c.Runs, c.Days, clusterDays, fmtSeconds(c.MedianOnSec), fmtSeconds(c.ShortOnSec), fmtSeconds(c.LongOnSec),
		gap, fmtEnergy(c.EnergyWh), strings.Join(hours, " "), with)
}

func fmtEnergy(wh float64) string {
	if wh < 1000 {
		return fmt.Sprintf("%.0f Wh", wh)
	}
	return fmt.Sprintf("%.2f kWh", wh/1000)
}

func reactiveRatio(s Signature) float64 {
	var q float64
	for _, v := range s.DQ {
		q += v
	}
	if s.PowerW == 0 {
		return 0
	}
	return math.Abs(q / s.PowerW)
}

func aiModel() string { return env("AI_MODEL", defaultAIModel) }

// aiPrompt builds the request: household description, appliances already named, and
// the unnamed signatures. Used for the API call and for pasting into a chat app.
func (s *Server) aiPrompt() (string, error) {
	s.mu.Lock()
	clusters := s.clusters()
	household := strings.TrimSpace(s.settings.Household)
	var named []string
	for _, a := range s.appliances {
		if a.Ignored {
			continue
		}
		var sigs []string
		for _, sg := range a.Signatures {
			d := describe(sg.DP, sg.DQ)
			sigs = append(sigs, fmt.Sprintf("%.0f W, %s, %s", d.PowerW, d.Legs, d.Character))
		}
		named = append(named, fmt.Sprintf("%s (%s)", a.Name, strings.Join(sigs, "; ")))
	}
	legs := len(s.detector.legs)
	s.mu.Unlock()

	if len(clusters) == 0 {
		return "", errors.New("no unnamed signatures to identify yet")
	}
	if len(clusters) > aiMaxClusters {
		clusters = clusters[:aiMaxClusters]
	}
	var b strings.Builder
	if household == "" {
		household = "(not provided)"
	}
	fmt.Fprintf(&b, "Household description from the owner:\n%s\n\n", household)
	if len(named) > 0 {
		fmt.Fprintf(&b, "Already identified (these signatures are taken):\n- %s\n\n", strings.Join(named, "\n- "))
	}
	fmt.Fprintf(&b, "Electrical service: %d legs/phases measured. Unidentified signatures from the last %d days:\n", legs, clusterDays)
	for _, c := range clusters {
		b.WriteString(describeForAI(c, legs) + "\n")
	}
	return b.String(), nil
}

// chatPrompt is the same request as plain text, for pasting into a chat app
// (e.g. the free claude.ai) instead of using the API.
func (s *Server) chatPrompt() (string, error) {
	prompt, err := s.aiPrompt()
	if err != nil {
		return "", err
	}
	return aiSystem + "\n\n" + prompt +
		"\nFor each key, list your guesses with confidence and reason. Use the key so I can find the signature on my dashboard.\n", nil
}

// AIProvider describes an API that can answer the request directly.
type AIProvider struct {
	ID    string `json:"id"` // "claude" or "gemini"
	Name  string `json:"name"`
	Model string `json:"model"`
}

// aiProviders lists the providers with credentials configured.
func aiProviders() []AIProvider {
	out := []AIProvider{}
	if aiEnabled() {
		out = append(out, AIProvider{"claude", "Claude", aiModel()})
	}
	if geminiEnabled() {
		out = append(out, AIProvider{"gemini", "Gemini", geminiModelSetting()})
	}
	return out
}

// askAI sends the request to a provider and stores the guesses per signature.
func (s *Server) askAI(ctx context.Context, provider string) (map[string]AISuggestion, error) {
	ask := map[string]func(context.Context, string) (string, error){"claude": askClaude, "gemini": askGemini}[provider]
	enabled := map[string]bool{"claude": aiEnabled(), "gemini": geminiEnabled()}[provider]
	switch {
	case ask == nil:
		return nil, fmt.Errorf("unknown AI provider %q", provider)
	case !enabled && provider == "claude":
		return nil, errors.New("set ANTHROPIC_API_KEY (or workload identity federation variables) to enable the Claude API")
	case !enabled:
		return nil, errors.New("set GEMINI_API_KEY to enable Gemini")
	}
	prompt, err := s.aiPrompt()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, aiRequestTimeout)
	defer cancel()
	text, err := ask(ctx, prompt)
	if err != nil {
		return nil, err
	}
	var out struct {
		Signatures []struct {
			Key     string    `json:"key"`
			Guesses []AIGuess `json:"guesses"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &out); err != nil {
		return nil, fmt.Errorf("unexpected AI response: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowMs()
	model := aiModel()
	if provider == "gemini" {
		model = geminiModelSetting() // the model that answered
	}
	for _, sig := range out.Signatures {
		s.aiSuggestions[sig.Key] = AISuggestion{At: now, Source: provider, Model: model, Guesses: sig.Guesses}
	}
	if err := s.writeJSON(aiFile, s.aiSuggestions, 0o644); err != nil {
		return nil, err
	}
	return s.aiSuggestions, nil
}

// askClaude sends the request to the Claude API with structured JSON output.
func askClaude(ctx context.Context, prompt string) (string, error) {
	model := aiModel()
	params := anthropic.BetaMessageNewParams{
		Model:     model,
		MaxTokens: 16000,
		System:    []anthropic.BetaTextBlockParam{{Text: aiSystem}},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(prompt)),
		},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: anthropic.BetaOutputConfigEffortMedium,
			Format: anthropic.BetaJSONOutputFormatParam{Schema: aiSchema},
		},
	}
	if !strings.Contains(model, "haiku") {
		// If a safety classifier declines, the API re-runs the request on a fallback model.
		// Haiku has no server-side fallback, so the parameter is only sent for other models.
		params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
	}
	client := anthropic.NewClient()
	resp, err := client.Beta.Messages.New(ctx, params)
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return "", fmt.Errorf("Claude API error %d: %s", apiErr.StatusCode, apiErr.Error())
		}
		return "", err
	}
	if resp.StopReason == anthropic.BetaStopReasonRefusal {
		return "", errors.New("the request was declined")
	}
	var text string
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			text += t.Text
		}
	}
	return text, nil
}
