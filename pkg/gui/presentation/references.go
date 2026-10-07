package presentation

import (
	"fmt"
	"strings"

	"github.com/fatih/color"
	"github.com/jesseduffield/lazydocker/pkg/commands"
	"github.com/jesseduffield/lazydocker/pkg/utils"
)

// RenderReferrers renders the reverse lookup of target: the containers that
// reference it, attributed to project and service. When the graph is not ready
// or some references could not be determined, that is reported explicitly
// rather than shown as "no references".
func RenderReferrers(graph *commands.RelationGraph, target commands.ObjectRef) string {
	if !graph.Ready() {
		return utils.ColoredString(
			"Reference data has not been collected yet. Nothing can be concluded about what uses this object.",
			color.FgYellow)
	}

	var output string
	refs := graph.Referrers(target)

	if len(refs) == 0 {
		output += utils.ColoredString("Not referenced by any container.", color.FgGreen) + "\n"
	} else {
		rows := make([][]string, 0, len(refs))
		for _, edge := range refs {
			rows = append(rows, []string{
				utils.ColoredString(edge.Referrer.Name, color.FgCyan),
				utils.ColoredString(orDash(edge.Project), color.FgYellow),
				utils.ColoredString(orDash(edge.Service), color.FgYellow),
				edge.Detail,
			})
		}
		table, err := utils.RenderTable(append([][]string{{"CONTAINER", "PROJECT", "SERVICE", "DETAIL"}}, rows...))
		if err != nil {
			return err.Error()
		}
		output += table + "\n"
	}

	output += renderUnknowns(graph.Uncertainties(kindForTarget(target.Type)))

	return output
}

// RenderReferences renders the forward lookup of ref: what the object uses.
// For containers this is its image, mounted volumes and connected networks.
func RenderReferences(graph *commands.RelationGraph, ref commands.ObjectRef) string {
	if !graph.Ready() {
		return utils.ColoredString(
			"Reference data has not been collected yet. Nothing can be concluded about what this object uses.",
			color.FgYellow)
	}

	edges := graph.References(ref)

	var output string
	if len(edges) == 0 {
		output += utils.ColoredString("This object has no recorded references.", color.FgGreen) + "\n"
	} else {
		rows := make([][]string, 0, len(edges))
		for _, edge := range edges {
			rows = append(rows, []string{
				utils.ColoredString(string(edge.Kind), color.FgMagenta),
				utils.ColoredString(edge.Target.Name, color.FgCyan),
				edge.Detail,
			})
		}
		table, err := utils.RenderTable(append([][]string{{"KIND", "TARGET", "DETAIL"}}, rows...))
		if err != nil {
			return err.Error()
		}
		output += table + "\n"
	}

	output += renderUnknownsForObject(graph, ref)

	return output
}

func renderUnknowns(unknowns []commands.Uncertainty) string {
	if len(unknowns) == 0 {
		return ""
	}
	lines := []string{utils.ColoredString("References could not be determined:", color.FgYellow)}
	for _, u := range unknowns {
		lines = append(lines, fmt.Sprintf("  %s %s",
			utils.ColoredString("!", color.FgRed), u.Reason))
	}
	return strings.Join(lines, "\n") + "\n"
}

func renderUnknownsForObject(graph *commands.RelationGraph, ref commands.ObjectRef) string {
	all := graph.Uncertainties("")
	var own []commands.Uncertainty
	for _, u := range all {
		if u.Object == ref {
			own = append(own, u)
		}
	}
	return renderUnknowns(own)
}

func kindForTarget(t commands.ObjectType) commands.ReferenceKind {
	switch t {
	case commands.ImageObject:
		return commands.ImageReference
	case commands.VolumeObject:
		return commands.VolumeReference
	case commands.NetworkObject:
		return commands.NetworkReference
	default:
		return ""
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
