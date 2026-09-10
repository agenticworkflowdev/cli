package workflow

import (
	"errors"
	"fmt"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/state"
)

type agentSessionRole string

const (
	specificationSessionRole  agentSessionRole = "specification"
	implementationSessionRole agentSessionRole = "implementation"
)

func manifestAgentSession(manifest state.Manifest, role agentSessionRole) string {
	if manifest.AgentSessions == nil {
		return ""
	}
	if role == specificationSessionRole {
		return manifest.AgentSessions.Specification
	}
	return manifest.AgentSessions.Implementation
}

func resumableAgentSession(manifest state.Manifest, role agentSessionRole, runner agent.Runner) (string, error) {
	sessionID := manifestAgentSession(manifest, role)
	if sessionID == "" {
		return "", nil
	}
	provider, err := agentRunnerProvider(runner)
	if err != nil {
		return "", err
	}
	if manifest.AgentSessions == nil || manifest.AgentSessions.Provider != provider {
		return "", fmt.Errorf("saved %s agent session belongs to provider %q, current provider is %q", role, manifest.AgentSessions.Provider, provider)
	}
	return sessionID, nil
}

func persistAgentSession(transition SpecificationTransitioner, controllerRoot string, manifest state.Manifest, role agentSessionRole, sessionID string, runner agent.Runner) (state.Manifest, error) {
	if sessionID == "" {
		return manifest, nil
	}
	provider, err := agentRunnerProvider(runner)
	if err != nil {
		return manifest, err
	}
	if manifest.AgentSessions != nil && manifest.AgentSessions.Provider != provider {
		return manifest, fmt.Errorf("saved agent sessions belong to provider %q, current provider is %q", manifest.AgentSessions.Provider, provider)
	}
	existing := manifestAgentSession(manifest, role)
	if existing != "" {
		if existing != sessionID {
			return manifest, fmt.Errorf("%s agent session changed from %q to %q", role, existing, sessionID)
		}
		return manifest, nil
	}
	if transition == nil {
		return manifest, errors.New("agent session transition is not configured")
	}
	updated := manifest
	if manifest.AgentSessions == nil {
		updated.AgentSessions = &state.AgentSessions{Provider: provider}
	} else {
		sessions := *manifest.AgentSessions
		updated.AgentSessions = &sessions
	}
	if role == specificationSessionRole {
		updated.AgentSessions.Specification = sessionID
	} else {
		updated.AgentSessions.Implementation = sessionID
	}
	if err := transition.Transition(controllerRoot, manifest.WorkflowID, updated); err != nil {
		return manifest, fmt.Errorf("persist %s agent session: %w", role, err)
	}
	return updated, nil
}

func agentRunnerProvider(runner agent.Runner) (agent.Provider, error) {
	providerRunner, ok := runner.(agent.ProviderRunner)
	if !ok {
		return "", errors.New("agent runner does not report its provider")
	}
	provider := providerRunner.Provider()
	if !provider.Valid() {
		return "", fmt.Errorf("agent runner reported unsupported provider %q", provider)
	}
	return provider, nil
}
