package retirementrelease

import (
	"encoding/json"
	"errors"
	"time"
)

// ActivationScope names the exact activation target and complete retirement set
// resolved from the owning trusted protocol configuration, never request input.
type ActivationScope struct {
	Pack           string
	Workflow       string
	Session        string
	RetiredScripts []string
}

// ProtocolPolicy supplies the owning workflow's explicit scope and time bounds.
// No policy, identity, script set, or duration is defaulted by this package.
type ProtocolPolicy struct {
	Reference            string
	Version              string
	Activation           ActivationScope
	MinimumTrialDuration time.Duration
	MaximumTrialToReview time.Duration
	MaximumContextAge    time.Duration
}

func (p *ProtocolPolicy) validate() error {
	if p == nil || !validAtom(p.Reference) || !validAtom(p.Version) || !validAtom(p.Activation.Pack) || !validAtom(p.Activation.Workflow) || !validAtom(p.Activation.Session) || len(p.Activation.RetiredScripts) == 0 || p.MinimumTrialDuration <= 0 || p.MaximumTrialToReview <= 0 || p.MaximumContextAge <= 0 {
		return errors.New("trusted protocol policy unavailable")
	}
	seen := map[string]bool{}
	for _, script := range p.Activation.RetiredScripts {
		if !relativePath(script) || seen[script] {
			return errors.New("trusted protocol retirement set invalid")
		}
		seen[script] = true
	}
	return nil
}

func (p *ProtocolPolicy) validateActivation(raw json.RawMessage) error {
	var activation struct {
		Pack     string   `json:"pack"`
		Workflow string   `json:"workflow"`
		Session  string   `json:"mayor_session"`
		Scripts  []string `json:"retired_scripts"`
	}
	if err := json.Unmarshal(raw, &activation); err != nil {
		return err
	}
	if activation.Pack != p.Activation.Pack || activation.Workflow != p.Activation.Workflow || activation.Session != p.Activation.Session || !equalSets(stringArray(activation.Scripts), p.Activation.RetiredScripts) {
		return errors.New("activation differs from configured exact protocol scope")
	}
	return nil
}
