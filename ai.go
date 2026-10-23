package main

// Optional AI suggestions for unnamed appliance signatures, using the Claude API
// (set ANTHROPIC_API_KEY to enable). Only summary numbers per signature and the
// household description the user wrote are sent; no raw readings or location.

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
	aiModel          = "claude-opus-5-5"
	aiFile           = "ai-suggestions.json"
	aiMaxClusters    = 15
	aiRequestTimeout = 3 * time.Minute
)

type AIGuess struct {
	Name       string `json:"name"`
	Confidence string `json:"confidence"` // high, medium, low
	Reason     string `json:"reason"`
}

type AISuggestion struct {
	At      int64     `json:"at"`
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
	return fmt.Sprintf("- key %s: %.0f W step, %s, %s (reactive/real %.2f); %d runs on %d of the last %d days; "+
		"on for %s typically (range %s-%s); starts %s; energy %.2f kWh in the period; start counts by hour (hh:count) %s",
		c.Key, c.Signature.PowerW, c.Signature.Legs, c.Signature.Character, reactiveRatio(c.Signature),
		c.Runs, c.Days, clusterDays, fmtSeconds(c.MedianOnSec), fmtSeconds(c.ShortOnSec), fmtSeconds(c.LongOnSec),
		gap, c.EnergyWh/1000, strings.Join(hours, " "))
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

// askAI requests suggestions for the unnamed clusters and stores them.
func (s *Server) askAI(ctx context.Context) (map[string]AISuggestion, error) {
	if !aiEnabled() {
		return nil, errors.New("set ANTHROPIC_API_KEY (or workload identity federation variables) to enable AI suggestions")
	}
	s.mu.Lock()
	clusters := s.clusters()
	household := strings.TrimSpace(s.settings.Household)
	var named []string
	for _, a := range s.appliances {
		named = append(named, fmt.Sprintf("%s (%s)", a.Name, func() string {
			d := describe(a.DP, a.DQ)
			return fmt.Sprintf("%.0f W, %s, %s", d.PowerW, d.Legs, d.Character)
		}()))
	}
	legs := len(s.detector.legs)
	s.mu.Unlock()

	if len(clusters) == 0 {
		return nil, errors.New("no unnamed signatures to identify yet")
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

	ctx, cancel := context.WithTimeout(ctx, aiRequestTimeout)
	defer cancel()
	client := anthropic.NewClient()
	resp, err := client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:     aiModel,
		MaxTokens: 16000,
		System:    []anthropic.BetaTextBlockParam{{Text: aiSystem}},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(b.String())),
		},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: anthropic.BetaOutputConfigEffortMedium,
			Format: anthropic.BetaJSONOutputFormatParam{Schema: aiSchema},
		},
		// If a safety classifier declines, the API re-runs the request on a fallback model.
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	})
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return nil, fmt.Errorf("Claude API error %d: %s", apiErr.StatusCode, apiErr.Error())
		}
		return nil, err
	}
	if resp.StopReason == anthropic.BetaStopReasonRefusal {
		return nil, errors.New("the request was declined")
	}
	var text string
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			text += t.Text
		}
	}
	var out struct {
		Signatures []struct {
			Key     string    `json:"key"`
			Guesses []AIGuess `json:"guesses"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("unexpected AI response: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowMs()
	for _, sig := range out.Signatures {
		s.aiSuggestions[sig.Key] = AISuggestion{At: now, Guesses: sig.Guesses}
	}
	if err := s.writeJSON(aiFile, s.aiSuggestions, 0o644); err != nil {
		return nil, err
	}
	return s.aiSuggestions, nil
}
