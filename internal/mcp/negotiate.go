package mcp

import (
	"fmt"
	"regexp"
	"strings"
)

// handshakeProtocolVersions lists the initialize-handshake protocol versions
// this client speaks, newest first. Revision 2026-07-28 retired the
// initialize handshake in favor of per-request metadata and is not supported
// by this client yet.
var handshakeProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

var (
	// protocolVersionPattern matches ISO date protocol version tokens in
	// server diagnostics such as "Unsupported protocol version '2024-11-05'.
	// Supported: 2025-11-25, 2025-06-18".
	protocolVersionPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	// versionRejectionPattern identifies initialize failures that are
	// protocol-version rejections rather than unrelated errors (auth, bad
	// params), so negotiation retries only the former.
	versionRejectionPattern = regexp.MustCompile(`(?i)protocol version`)
)

func supportsProtocolVersion(version string) bool {
	for _, candidate := range handshakeProtocolVersions {
		if candidate == version {
			return true
		}
	}
	return false
}

func looksLikeVersionRejection(diagnostics string) bool {
	return versionRejectionPattern.MatchString(diagnostics)
}

// retryInitializeVersion picks the next protocol version to propose after an
// initialize attempt failed with server diagnostics. Servers that reject a
// proposal either counter-offer in the result (legacy revisions) or answer
// with an "Unsupported protocol version" error listing what they support
// (newer revisions, including HTTP 400 bodies); both paths surface version
// tokens as text. Prefer the newest version the server listed that we support
// and have not tried, so a server that only speaks old versions still gets
// its newest rather than ours. Fall back to our next-oldest candidate when
// the rejection names none.
func retryInitializeVersion(diagnostics string, tried map[string]bool) string {
	listed := map[string]bool{}
	for _, match := range protocolVersionPattern.FindAllString(diagnostics, -1) {
		listed[match] = true
	}
	var newestMatched string
	for _, candidate := range handshakeProtocolVersions {
		if listed[candidate] && !tried[candidate] {
			// The list is newest-first, so the first hit is the newest.
			newestMatched = candidate
			break
		}
	}
	if newestMatched != "" {
		return newestMatched
	}
	if len(listed) > 0 {
		// The server named versions and none of them are ours and untried:
		// there is nothing left to negotiate.
		return ""
	}
	for _, candidate := range handshakeProtocolVersions {
		if !tried[candidate] {
			return candidate
		}
	}
	return ""
}

// initializeHandshake performs the MCP initialize handshake under protocol
// version negotiation. send issues the initialize request for a proposed
// version and returns the version echoed by the server (possibly empty) and
// any error; notifyInitialized completes the handshake. The negotiated
// version is returned.
func initializeHandshake(send func(version string) (string, error), notifyInitialized func(version string) error) (string, error) {
	tried := map[string]bool{}
	version := handshakeProtocolVersions[0]
	for i := 0; i < len(handshakeProtocolVersions); i++ {
		tried[version] = true
		echo, err := send(version)
		if err == nil {
			if echo != "" && echo != version && supportsProtocolVersion(echo) {
				version = echo
			}
			if err := notifyInitialized(version); err != nil {
				return "", err
			}
			return version, nil
		}
		if !looksLikeVersionRejection(err.Error()) {
			return "", err
		}
		next := retryInitializeVersion(err.Error(), tried)
		if next == "" {
			return "", fmt.Errorf("initialize MCP handshake: no mutually supported protocol version: %w", err)
		}
		version = next
	}
	return "", fmt.Errorf("initialize MCP handshake: no mutually supported protocol version")
}

func initializeParams(version string) map[string]any {
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "zero",
			"version": "dev",
		},
	}
}

// mcpErrorMessage renders a JSON-RPC error for the caller. The error's data
// member is appended because servers place the decisive detail there — for
// example "Unsupported protocol version '...'. Supported: ..." — where both
// users and version negotiation need to see it.
func mcpErrorMessage(method string, failure *rpcError) error {
	detail := strings.TrimSpace(failure.Message)
	data := strings.TrimSpace(string(failure.Data))
	if data != "" && data != "null" {
		if detail == "" {
			detail = data
		} else {
			detail = detail + ": " + data
		}
	}
	return fmt.Errorf("MCP %s failed: %s", method, detail)
}
