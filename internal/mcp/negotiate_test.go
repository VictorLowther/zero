package mcp

import (
	"errors"
	"strings"
	"testing"
)

func TestRetryInitializeVersionParsesSupportedList(t *testing.T) {
	// A newer server rejects our proposal and lists what it speaks, in the
	// shape the 2025-era servers emit ("Unsupported protocol version '...'.
	// Supported: ..."), including a modern version we do not speak.
	diagnostics := "Invalid params: {\"params\":\"Unsupported protocol version '2025-11-25'. Supported: 2026-07-28, 2025-11-25, 2025-06-18, 2025-03-26\"}"
	tried := map[string]bool{"2025-11-25": true}
	if got := retryInitializeVersion(diagnostics, tried); got != "2025-06-18" {
		t.Fatalf("retryInitializeVersion() = %q, want 2025-06-18 (newest listed version we support and have not tried)", got)
	}
}

func TestRetryInitializeVersionPrefersServerNewest(t *testing.T) {
	// The server only speaks older revisions; pick its newest, not ours.
	diagnostics := "Unsupported protocol version '2025-11-25'. Supported: 2025-03-26, 2024-11-05"
	tried := map[string]bool{"2025-11-25": true}
	if got := retryInitializeVersion(diagnostics, tried); got != "2025-03-26" {
		t.Fatalf("retryInitializeVersion() = %q, want 2025-03-26", got)
	}
}

func TestRetryInitializeVersionNoCommonVersion(t *testing.T) {
	// The server named versions, but none are ones we speak: stop rather
	// than burn every candidate on certain rejections.
	diagnostics := "Unsupported protocol version '2025-11-25'. Supported: 2026-07-28"
	tried := map[string]bool{"2025-11-25": true}
	if got := retryInitializeVersion(diagnostics, tried); got != "" {
		t.Fatalf("retryInitializeVersion() = %q, want empty (no mutually supported version)", got)
	}
}

func TestRetryInitializeVersionGenericRejectionFallsToNextCandidate(t *testing.T) {
	// A version rejection that names no versions falls back to our next
	// oldest candidate.
	diagnostics := "Requested protocol version is not supported."
	tried := map[string]bool{"2025-11-25": true}
	if got := retryInitializeVersion(diagnostics, tried); got != "2025-06-18" {
		t.Fatalf("retryInitializeVersion() = %q, want 2025-06-18", got)
	}
}

func TestRetryInitializeVersionExhaustsCandidates(t *testing.T) {
	tried := map[string]bool{
		"2025-11-25": true, "2025-06-18": true, "2025-03-26": true, "2024-11-05": true,
	}
	if got := retryInitializeVersion("Unsupported protocol version. Supported: whatever", tried); got != "" {
		t.Fatalf("retryInitializeVersion() = %q, want empty after trying every candidate", got)
	}
}

func TestLooksLikeVersionRejection(t *testing.T) {
	if !looksLikeVersionRejection("MCP initialize failed: Invalid params: Unsupported protocol version '2024-11-05'") {
		t.Fatal("expected unsupported protocol version text to be treated as a version rejection")
	}
	if looksLikeVersionRejection("MCP initialize failed: unauthorized") {
		t.Fatal("unrelated errors must not look like version rejections")
	}
}

func TestInitializeHandshakeAdoptsCounterOffer(t *testing.T) {
	var notified string
	version, err := initializeHandshake(
		func(proposed string) (string, error) {
			if proposed != handshakeProtocolVersions[0] {
				t.Fatalf("proposed %q, want the newest candidate %q", proposed, handshakeProtocolVersions[0])
			}
			return "2024-11-05", nil
		},
		func(negotiated string) error {
			notified = negotiated
			return nil
		},
	)
	if err != nil {
		t.Fatalf("initializeHandshake() error = %v", err)
	}
	if version != "2024-11-05" || notified != "2024-11-05" {
		t.Fatalf("version = %q, notified = %q, want the server counter-offer", version, notified)
	}
}

func TestInitializeHandshakeRetriesOnUnsupportedVersion(t *testing.T) {
	var proposals []string
	var notified string
	version, err := initializeHandshake(
		func(proposed string) (string, error) {
			proposals = append(proposals, proposed)
			if proposed != "2025-03-26" {
				return "", errors.New("MCP initialize failed: Invalid params: {\"params\":\"Unsupported protocol version '" + proposed + "'. Supported: 2025-11-25, 2025-06-18, 2025-03-26\"}")
			}
			return "2025-03-26", nil
		},
		func(negotiated string) error {
			notified = negotiated
			return nil
		},
	)
	if err != nil {
		t.Fatalf("initializeHandshake() error = %v", err)
	}
	want := []string{"2025-11-25", "2025-06-18", "2025-03-26"}
	if strings.Join(proposals, ",") != strings.Join(want, ",") {
		t.Fatalf("proposals = %v, want %v", proposals, want)
	}
	if version != "2025-03-26" || notified != "2025-03-26" {
		t.Fatalf("version = %q, notified = %q, want 2025-03-26", version, notified)
	}
}

func TestInitializeHandshakeStopsWithoutCommonVersion(t *testing.T) {
	proposals := 0
	_, err := initializeHandshake(
		func(proposed string) (string, error) {
			proposals++
			return "", errors.New("MCP initialize failed: Invalid params: Unsupported protocol version '" + proposed + "'. Supported: 2026-07-28")
		},
		func(string) error { return nil },
	)
	if err == nil {
		t.Fatal("expected an error when no protocol version is mutually supported")
	}
	if proposals != 1 {
		t.Fatalf("proposals = %d, want 1 (a server naming only unsupported versions ends negotiation immediately)", proposals)
	}
}

func TestInitializeHandshakePropagatesNonVersionErrors(t *testing.T) {
	proposals := 0
	_, err := initializeHandshake(
		func(string) (string, error) {
			proposals++
			return "", errors.New("MCP initialize failed: unauthorized")
		},
		func(string) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("error = %v, want the unauthorized failure", err)
	}
	if proposals != 1 {
		t.Fatalf("proposals = %d, want 1 (non-version errors must not trigger retries)", proposals)
	}
}

func TestMCPErrorMessageIncludesData(t *testing.T) {
	err := mcpErrorMessage("initialize", &rpcError{
		Code:    -32602,
		Message: "Invalid params",
		Data:    mustRaw(map[string]any{"params": "Unsupported protocol version '2024-11-05'. Supported: 2025-11-25"}),
	})
	message := err.Error()
	if !strings.Contains(message, "Invalid params") || !strings.Contains(message, "Unsupported protocol version") {
		t.Fatalf("error = %q, want both message and data detail", message)
	}
}

func TestMCPErrorMessageWithoutData(t *testing.T) {
	err := mcpErrorMessage("tools/call", &rpcError{Code: -32601, Message: "method not found"})
	if err.Error() != "MCP tools/call failed: method not found" {
		t.Fatalf("error = %q, want message-only rendering", err.Error())
	}
}
