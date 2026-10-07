package commands

import (
	"fmt"
	"strings"

	"github.com/jesseduffield/lazydocker/pkg/config"
)

// DeleteAction is the resolved policy action for an object.
type DeleteAction string

const (
	// PolicyLegacy means no policy applies: deletion behaves exactly as it did
	// before this feature existed.
	PolicyLegacy DeleteAction = ""
	// PolicyReject refuses deletion and lists the referrers.
	PolicyReject DeleteAction = config.DeleteActionReject
	// PolicyWarn asks for confirmation with the referrers listed and then
	// continues.
	PolicyWarn DeleteAction = config.DeleteActionWarn
	// PolicyCascade removes referencing objects layer by layer and then the
	// target.
	PolicyCascade DeleteAction = config.DeleteActionCascade
)

// DeletePlan is the pre-deletion judgment for one object.
type DeletePlan struct {
	Target ObjectRef
	Action DeleteAction

	// Referrers are the objects referencing Target (reverse lookup)
	Referrers []Reference
	// Unknowns are the references that could not be determined but could
	// point at Target
	Unknowns []Uncertainty

	// Layers groups the cascade closure by depth: Layers[0] is the target and
	// each following layer contains the referrers of the previous layer.
	Layers [][]ObjectRef

	// projectByReferrer attributes nodes in the cascade to their compose
	// project, so each layer can be judged against its own policy.
	projectByReferrer map[string]string
}

// Blocked reports whether the plan refuses deletion: either referrers exist
// or relevant references could not be determined.
func (p DeletePlan) Blocked() bool {
	return len(p.Referrers) > 0 || len(p.Unknowns) > 0
}

// PlanDelete performs the pre-deletion judgment for target. projectHint is the
// project the target belongs to (used for the project dimension of the
// policy); pass "" for unlabeled/global objects.
func (c *DockerCommand) PlanDelete(target ObjectRef, projectHint string) DeletePlan {
	graph := c.Relations()
	plan := DeletePlan{
		Target:            target,
		Action:            c.resolveDeleteAction(target.Type, projectHint),
		projectByReferrer: make(map[string]string),
	}

	if graph == nil {
		// Reference data has never been collected. With no policy the legacy
		// request is still sent; any configured action must treat the
		// situation as unknown, not as "no references".
		if plan.Action != PolicyLegacy {
			plan.Unknowns = []Uncertainty{{
				Object: target,
				Reason: "reference data has not been collected yet",
			}}
		}
		return plan
	}

	for _, edge := range graph.Referrers(target) {
		plan.Referrers = append(plan.Referrers, edge)
		plan.projectByReferrer[edge.Referrer.ID] = edge.Project
	}
	plan.Unknowns = graph.DeleteUnknowns(target)
	plan.Layers = buildLayers(graph, target)

	return plan
}

// RemoveFunc removes the given object. The caller supplies the concrete
// removal for each node type in the cascade.
type RemoveFunc func(ObjectRef) error

// ExecuteDelete runs the plan. For cascade plans the referrers are removed
// layer by layer (deepest layer first) and then the target; each referrer is
// itself judged against the policy for its type and project.
func (c *DockerCommand) ExecuteDelete(plan DeletePlan, remove RemoveFunc) error {
	switch plan.Action {
	case PolicyReject:
		if plan.Blocked() {
			return &ReferenceBlockError{
				Target:    plan.Target,
				Action:    PolicyReject,
				Referrers: plan.Referrers,
				Unknowns:  plan.Unknowns,
			}
		}
		return remove(plan.Target)
	case PolicyCascade:
		return c.executeCascade(plan, remove)
	default:
		return remove(plan.Target)
	}
}

func (c *DockerCommand) executeCascade(plan DeletePlan, remove RemoveFunc) error {
	// Under a cascade, referrers are expected — they are exactly what gets
	// reclaimed. Only unresolved/unknown references block the cascade since
	// they must never be silently treated as "nothing to reclaim".
	if len(plan.Unknowns) > 0 {
		return &ReferenceBlockError{
			Target:    plan.Target,
			Action:    PolicyCascade,
			Referrers: plan.Referrers,
			Unknowns:  plan.Unknowns,
		}
	}

	for layerIdx := len(plan.Layers) - 1; layerIdx >= 1; layerIdx-- {
		layer := plan.Layers[layerIdx]
		for _, ref := range layer {
			project := plan.projectByReferrer[ref.ID]
			sub := c.PlanDelete(ref, project)

			// A "reject" policy on a cascade node stops the whole cascade and
			// reports what is blocking it; warn/legacy proceed.
			if sub.Action == PolicyReject && sub.Blocked() {
				return &ReferenceBlockError{
					Target:    ref,
					Action:    PolicyReject,
					Referrers: sub.Referrers,
					Unknowns:  sub.Unknowns,
				}
			}

			if err := remove(ref); err != nil {
				return &NodeRemoveError{Node: ref, Err: err}
			}
		}
	}

	if err := remove(plan.Target); err != nil {
		return &NodeRemoveError{Node: plan.Target, Err: err}
	}
	return nil
}

func buildLayers(graph *RelationGraph, target ObjectRef) [][]ObjectRef {
	visited := map[ObjectRef]bool{target: true}
	layers := [][]ObjectRef{{target}}

	for depth := 0; ; depth++ {
		var next []ObjectRef
		for _, node := range layers[depth] {
			for _, edge := range graph.Referrers(node) {
				if visited[edge.Referrer] {
					continue
				}
				visited[edge.Referrer] = true
				next = append(next, edge.Referrer)
			}
		}
		if len(next) == 0 {
			break
		}
		layers = append(layers, next)
	}
	return layers
}

// ---- batch cleanup ----

// BatchTarget pairs an object with the project it belongs to for policy
// resolution.
type BatchTarget struct {
	Target  ObjectRef
	Project string
}

// PlanBatch performs pre-deletion judgments for a batch cleanup.
func (c *DockerCommand) PlanBatch(targets []BatchTarget) []DeletePlan {
	plans := make([]DeletePlan, 0, len(targets))
	for _, t := range targets {
		plans = append(plans, c.PlanDelete(t.Target, t.Project))
	}
	return plans
}

// AllPlansLegacy reports whether no policy applies to any item. When true the
// caller can run the exact pre-feature code path.
func AllPlansLegacy(plans []DeletePlan) bool {
	for _, p := range plans {
		if p.Action != PolicyLegacy {
			return false
		}
	}
	return true
}

// BatchFailure records an object whose removal failed during a batch.
type BatchFailure struct {
	Target ObjectRef
	Err    error
}

// BatchReport is the outcome of a batch cleanup.
type BatchReport struct {
	Removed int
	// Skipped are objects rejected by policy (their plans carry referrers)
	Skipped []DeletePlan
	// Failed are objects whose removal errored
	Failed []BatchFailure
}

// HasIssues reports whether any item was skipped or failed.
func (r BatchReport) HasIssues() bool {
	return len(r.Skipped) > 0 || len(r.Failed) > 0
}

// ExecuteBatch runs each plan independently: a rejected object is skipped and
// listed, other removals run even if earlier ones errored — so one failure
// never aborts the rest of the cleanup.
func (c *DockerCommand) ExecuteBatch(plans []DeletePlan, remove RemoveFunc) BatchReport {
	report := BatchReport{}
	for _, plan := range plans {
		if plan.Action == PolicyReject && plan.Blocked() {
			report.Skipped = append(report.Skipped, plan)
			continue
		}
		if err := c.ExecuteDelete(plan, remove); err != nil {
			report.Failed = append(report.Failed, BatchFailure{Target: plan.Target, Err: err})
			continue
		}
		report.Removed++
	}
	return report
}

// ---- policy resolution ----

func (c *DockerCommand) resolveDeleteAction(objectType ObjectType, project string) DeleteAction {
	p := c.Config.UserConfig.DeletePolicy
	action := resolvePolicy(p, objectType, project)
	switch action {
	case PolicyReject, PolicyWarn, PolicyCascade:
		return action
	default:
		// Unknown action strings fall back to legacy behavior rather than
		// silently changing it.
		return PolicyLegacy
	}
}

// resolvePolicy resolves the action for (objectType, project).
// Precedence (highest first):
//  1. a rule matching both type and project
//  2. a rule matching one of them
//  3. ByProject
//  4. ByType
//  5. a rule matching everything (type and project empty)
//  6. Default
//  7. legacy (no check)
func resolvePolicy(p config.DeletePolicyConfig, objectType ObjectType, project string) DeleteAction {
	var singleMatch, catchAll string
	for _, rule := range p.Rules {
		typeMatches := rule.ObjectType == "" || normalizeTypeName(rule.ObjectType) == string(objectType)
		projectMatches := rule.Project == "" || rule.Project == project
		if !typeMatches || !projectMatches {
			continue
		}
		switch {
		case rule.ObjectType != "" && rule.Project != "":
			return DeleteAction(rule.Action)
		case rule.ObjectType != "" || rule.Project != "":
			if singleMatch == "" {
				singleMatch = rule.Action
			}
		default:
			if catchAll == "" {
				catchAll = rule.Action
			}
		}
	}
	if singleMatch != "" {
		return DeleteAction(singleMatch)
	}
	if project != "" {
		if action, ok := p.ByProject[project]; ok {
			return DeleteAction(action)
		}
	}
	if action, ok := p.ByType[string(objectType)]; ok {
		return DeleteAction(action)
	}
	if catchAll != "" {
		return DeleteAction(catchAll)
	}
	if p.Default != "" {
		return DeleteAction(p.Default)
	}
	return PolicyLegacy
}

// normalizeTypeName accepts singular ("image") and plural ("images") type
// names from the user config.
func normalizeTypeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ToLower(name)
	return strings.TrimSuffix(name, "s")
}

// ReferenceBlockError is returned when deletion is refused because of
// references or uncertainty.
type ReferenceBlockError struct {
	Target    ObjectRef
	Action    DeleteAction
	Referrers []Reference
	Unknowns  []Uncertainty
}

func (e *ReferenceBlockError) Error() string {
	parts := []string{fmt.Sprintf("Deletion of %s blocked by policy", e.Target)}
	if len(e.Referrers) > 0 {
		parts = append(parts, "Referenced by:")
		for _, edge := range e.Referrers {
			parts = append(parts, "  - "+edgeLine(edge))
		}
	}
	if len(e.Unknowns) > 0 {
		parts = append(parts, "References could not be determined:")
		for _, u := range e.Unknowns {
			parts = append(parts, "  - "+uncertaintyLine(u))
		}
	}
	return strings.Join(parts, "\n")
}

// NodeRemoveError wraps a removal error for a specific cascade node.
type NodeRemoveError struct {
	Node ObjectRef
	Err  error
}

func (e *NodeRemoveError) Error() string {
	return fmt.Sprintf("failed to remove %s: %v", e.Node, e.Err)
}

func (e *NodeRemoveError) Unwrap() error {
	return e.Err
}

func edgeLine(edge Reference) string {
	contextParts := make([]string, 0, 2)
	if edge.Project != "" {
		contextParts = append(contextParts, "project: "+edge.Project)
	}
	if edge.Service != "" {
		contextParts = append(contextParts, "service: "+edge.Service)
	}
	context := ""
	if len(contextParts) > 0 {
		context = " (" + strings.Join(contextParts, ", ") + ")"
	}
	return edge.Referrer.String() + context
}

func uncertaintyLine(u Uncertainty) string {
	target := u.Object.String()
	if u.Kind != "" {
		target += " [" + string(u.Kind) + "]"
	}
	return target + ": " + u.Reason
}
