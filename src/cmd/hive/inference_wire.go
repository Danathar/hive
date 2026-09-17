package main

import (
	"context"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inference"
)

// This file is the thin wiring layer between the hive binary and the inference
// routing policy, which now lives in pkg/inference (#7238 stage 3).

func litellmLocalProxyURL() string {
	return inference.LocalLiteLLMProxyURL()
}

func resolveLiteLLMInferenceRoute(cfg *config.Config, backend, requestedModel string) (endpoint, model string, ok bool) {
	return inference.ResolveLiteLLMRoute(cfg, backend, requestedModel)
}

func resolveWatsonxGateway(cfg *config.Config) *config.GatewayConfig {
	return inference.ResolveWatsonxGateway(cfg)
}

func resolveGatewayAuth(gw *config.GatewayConfig, agentName, backend string, logger *slog.Logger) (string, map[string]string) {
	return inference.ResolveGatewayAuth(gw, agentName, backend, logger)
}

func superviseLocalLiteLLM(ctx context.Context, logger *slog.Logger) {
	inference.SuperviseLocalLiteLLM(ctx, logger)
}

// parseEndpointList splits a comma-separated list of URLs into a slice.
// A single URL is returned as a one-element slice.
func parseEndpointList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
