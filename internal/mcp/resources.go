package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"relay/internal/registry"
	"relay/pkg/relay"
)

// Skill exposures over MCP's standard resource mechanism (spec §29). MCP sees
// the same skill the CLI does because neither surface owns the text: both ask
// the installed binary, the CLI directly and MCP through the daemon.
const (
	// SkillURIPrefix is the resource URI scheme for embedded skills:
	// relay://skill/<tool>.
	SkillURIPrefix = "relay://skill/"
	// SkillMIMEType is the media type of a SKILL.md.
	SkillMIMEType = "text/markdown"
)

// SkillResource describes one tool skill advertised in resources/list.
type SkillResource struct {
	// Tool is the registered tool name, e.g. "github".
	Tool string
	// Name is the human-readable resource name shown to the client.
	Name string
	// Description is a one-line summary of the resource.
	Description string
}

// SkillSource lists the skills this server advertises and reads their text.
//
// It is a narrow seam so the resources handlers can be tested without a daemon
// or installed tools. The production implementation reads through the daemon,
// which is the only component allowed to run an installed binary (spec §27), so
// MCP gains no execution path of its own.
type SkillSource interface {
	ListSkills(ctx context.Context) ([]SkillResource, error)
	ReadSkill(ctx context.Context, tool string) (string, *relay.Error)
}

// SkillURI builds the MCP resource URI for one tool's skill.
func SkillURI(tool string) string { return SkillURIPrefix + tool }

// ParseSkillURI extracts the tool name from a skill resource URI. It reports
// false for any other URI and for a name that could never be registered, so a
// crafted URI cannot be turned into a registry or binary-path lookup.
func ParseSkillURI(uri string) (string, bool) {
	if !strings.HasPrefix(uri, SkillURIPrefix) {
		return "", false
	}
	tool := strings.TrimPrefix(uri, SkillURIPrefix)
	if !registry.ValidName(tool) {
		return "", false
	}
	return tool, true
}

// handleResourcesList advertises each registered tool that embeds a skill
// (spec §8, §29). Listing reports discovery metadata only; the text is read on
// demand by resources/read, so listing never executes a binary.
func (s *Server) handleResourcesList(ctx context.Context, msg *rpcRequest, w io.Writer) {
	resources := []map[string]any{}
	if s.Skills != nil {
		skills, err := s.Skills.ListSkills(ctx)
		if err != nil {
			s.sendError(w, msg.ID, CodeInternalError, "failed to list resources: "+err.Error())
			return
		}
		for _, skill := range skills {
			resources = append(resources, map[string]any{
				"uri":         SkillURI(skill.Tool),
				"name":        skill.Name,
				"description": skill.Description,
				"mimeType":    SkillMIMEType,
			})
		}
	}
	s.sendResult(w, msg.ID, map[string]any{"resources": resources})
}

// handleResourcesRead returns one tool's embedded SKILL.md (spec §8, §29).
//
// The text comes from the installed binary through the daemon on every read, so
// MCP and the CLI see identical bytes and no cached copy can drift (spec §16).
// An unknown tool, or a tool with no skill, is answered with the same structured
// error the CLI would print rather than a protocol crash (spec §26).
func (s *Server) handleResourcesRead(ctx context.Context, msg *rpcRequest, w io.Writer) {
	if msg.Params == nil {
		s.sendError(w, msg.ID, CodeInvalidParams, "missing params")
		return
	}
	var params struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(*msg.Params, &params); err != nil {
		s.sendError(w, msg.ID, CodeInvalidParams, "invalid params: "+err.Error())
		return
	}
	if params.URI == "" {
		s.sendError(w, msg.ID, CodeInvalidParams, "missing resource uri")
		return
	}

	tool, ok := ParseSkillURI(params.URI)
	if !ok {
		s.sendStructuredError(w, msg.ID, CodeInvalidParams,
			relay.NewError(relay.CodeInvalidInput, "unknown resource uri: "+params.URI))
		return
	}
	if s.Skills == nil {
		s.sendStructuredError(w, msg.ID, CodeInvalidParams,
			relay.NewError(relay.CodeToolNotFound, fmt.Sprintf("tool %q is not registered", tool)))
		return
	}

	text, appErr := s.Skills.ReadSkill(ctx, tool)
	if appErr != nil {
		s.sendStructuredError(w, msg.ID, CodeInvalidParams, appErr)
		return
	}

	s.sendResult(w, msg.ID, map[string]any{
		"contents": []map[string]any{{
			"uri":      params.URI,
			"mimeType": SkillMIMEType,
			"text":     text,
		}},
	})
}

// sendStructuredError writes a JSON-RPC protocol error that preserves the
// structured relay error in "data" (spec §26, §29). MCP requires a read failure
// to be a protocol fault rather than a tool result, so the relay code travels
// alongside it and a client can branch on the same value the CLI prints.
func (s *Server) sendStructuredError(w io.Writer, id any, code int, err *relay.Error) {
	s.writeFrame(w, rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: err.Error(),
			Data:    err,
		},
	})
}
