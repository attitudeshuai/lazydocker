package presentation

import (
	"fmt"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/jesseduffield/lazydocker/pkg/commands"
	"github.com/jesseduffield/lazydocker/pkg/i18n"
	"github.com/jesseduffield/lazydocker/pkg/utils"
)

// StatsStateText localises a collector lifecycle state.
func StatsStateText(tr *i18n.TranslationSet, state commands.StatsState) string {
	switch state {
	case commands.StatsStateIdle:
		return tr.StatsStateIdle
	case commands.StatsStateConnecting:
		return tr.StatsStateConnecting
	case commands.StatsStateRunning:
		return tr.StatsStateRunning
	case commands.StatsStateRetrying:
		return tr.StatsStateRetrying
	case commands.StatsStatePaused:
		return tr.StatsStatePaused
	case commands.StatsStateStopped:
		return tr.StatsStateStopped
	default:
		return string(state)
	}
}

func statsStateColor(state commands.StatsState) color.Attribute {
	switch state {
	case commands.StatsStateRunning:
		return color.FgGreen
	case commands.StatsStateConnecting:
		return color.FgCyan
	case commands.StatsStateRetrying, commands.StatsStatePaused, commands.StatsStateStopped:
		return color.FgYellow
	default:
		return color.FgWhite
	}
}

// RenderStatsStatusLine renders the observable collection state of one
// container, including retry/backoff details while it is reconnecting.
func RenderStatsStatusLine(tr *i18n.TranslationSet, status commands.StatsStatus) string {
	line := fmt.Sprintf("%s: %s", tr.StatsCollectionLabel, StatsStateText(tr, status.State))

	if status.State == commands.StatsStateRetrying {
		details := []string{fmt.Sprintf("%s %d", tr.StatsAttemptLabel, status.Attempts)}
		if !status.NextRetryAt.IsZero() {
			remaining := time.Until(status.NextRetryAt).Round(time.Second)
			if remaining < 0 {
				remaining = 0
			}
			details = append(details, fmt.Sprintf("%s %s", tr.StatsNextRetryLabel, remaining))
		}
		if status.LastError != "" {
			details = append(details, status.LastError)
		}
		line += " (" + strings.Join(details, ", ") + ")"
	}

	return utils.ColoredString(line, statsStateColor(status.State))
}

// RenderAggregate renders the service/project level summary of the latest
// stats across a scope. It adds information without touching the values used
// by the per-container graphs.
func RenderAggregate(tr *i18n.TranslationSet, title string, aggregate commands.StatsAggregate) string {
	if aggregate.Members == 0 {
		return utils.ColoredString(title, color.FgCyan) + "\n" + tr.StatsAggregateEmpty
	}

	sections := []string{utils.ColoredString(title, color.FgCyan)}

	memberSummary := fmt.Sprintf("%s: %d (%s: %d",
		tr.StatsContainersLabel, aggregate.Members, tr.StatsReportingLabel, aggregate.Reporting)
	for _, state := range []commands.StatsState{
		commands.StatsStateRunning,
		commands.StatsStateConnecting,
		commands.StatsStateRetrying,
		commands.StatsStatePaused,
		commands.StatsStateStopped,
		commands.StatsStateIdle,
	} {
		count := aggregate.CollectorStates[state]
		if count > 0 {
			memberSummary += fmt.Sprintf(", %s: %d", StatsStateText(tr, state), count)
		}
	}
	memberSummary += ")"
	sections = append(sections, memberSummary)

	sections = append(sections,
		fmt.Sprintf("%s: %0.2f%%    %s: %0.2f%%",
			tr.StatsCPUTotalLabel, aggregate.CPUPercentageSum,
			tr.StatsCPUAverageLabel, aggregate.CPUPercentageAvg),
		fmt.Sprintf("%s: %0.2f%%    %s: %0.2f%%",
			tr.StatsMemoryTotalLabel, aggregate.MemoryPercentageSum,
			tr.StatsMemoryAverageLabel, aggregate.MemoryPercentageAvg),
	)

	if aggregate.MemoryLimit > 0 {
		sections = append(sections, fmt.Sprintf("%s: %s / %s",
			tr.StatsMemoryUsageLabel,
			utils.FormatDecimalBytes(int(aggregate.MemoryUsage)),
			utils.FormatDecimalBytes(int(aggregate.MemoryLimit)),
		))
	}

	sections = append(sections, fmt.Sprintf("%s: %s    %s: %s",
		tr.StatsTrafficReceivedLabel, utils.FormatDecimalBytes(int(aggregate.NetworkRx)),
		tr.StatsTrafficSentLabel, utils.FormatDecimalBytes(int(aggregate.NetworkTx)),
	))

	return strings.Join(sections, "\n")
}

// RenderProjectStats renders the project level summary followed by a table of
// the latest stats for each member container.
func RenderProjectStats(tr *i18n.TranslationSet, projectName string, aggregate commands.StatsAggregate, members []*commands.Container) string {
	title := utils.ColoredString(
		fmt.Sprintf("%s — %s", projectName, tr.StatsSummaryTitle), color.FgCyan)

	sections := []string{RenderAggregate(tr, title, aggregate)}

	if len(members) > 0 {
		rows := make([][]string, 0, len(members)+1)
		rows = append(rows, []string{
			tr.StatsContainersLabel,
			tr.StatsStateRunning, // reused as a generic "state" column header
			"CPU%",
			"MEM%",
			tr.StatsMemoryUsageLabel,
		})
		for _, c := range members {
			rows = append(rows, projectStatsRow(c))
		}
		if table, err := utils.RenderTable(rows); err == nil {
			sections = append(sections, table)
		}
	}

	return strings.Join(sections, "\n\n")
}

func projectStatsRow(c *commands.Container) []string {
	row := []string{c.Name, c.Container.State, "", "", ""}

	if stats, ok := c.GetLastStats(); ok {
		row[2] = fmt.Sprintf("%0.2f%%", stats.DerivedStats.CPUPercentage)
		row[3] = fmt.Sprintf("%0.2f%%", stats.DerivedStats.MemoryPercentage)
		row[4] = utils.FormatDecimalBytes(stats.ClientStats.MemoryStats.Usage)
	}

	return row
}
