// Copyright 2026 Henry Zektser.

package cli

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/mcpdoll/mcpdoll/internal/api"
)

// Timed work, as commands (ADR 0026).
//
// The question these answer is "what does this system do when nobody is
// asking it to", which used to be answerable only by reading the source.

func newSchedulesCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedules",
		Short: "What the control plane does on its own, and how often",
		Long: "Timed work is a row rather than a hardcoded ticker, so a cadence\n" +
			"is something you change rather than deploy.\n\n" +
			"The data plane's own timers — backend health probing, drift scanning —\n" +
			"are deliberately not here. They live in its config file, because a data\n" +
			"plane that needed this database to keep probing would stop working\n" +
			"during exactly the control-plane outage it is built to survive.",
	}
	cmd.AddCommand(
		newSchedulesListCmd(env),
		newSchedulesShowCmd(env),
		newSchedulesSetCmd(env),
		newSchedulesRunCmd(env),
	)
	return cmd
}

func newSchedulesListCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "Every scheduled job, enabled or not",
		Annotations: map[string]string{annotationOperation: "listSchedules"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			var out api.ScheduleList
			if err := apiCall(ctx, env, "GET", "/api/v1/schedules", nil, &out); err != nil {
				return err
			}
			return env.Emit(scheduleListReport(out))
		},
	}
}

func newSchedulesShowCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:         "show <job-type>",
		Short:       "One scheduled job",
		Annotations: map[string]string{annotationOperation: "getSchedule"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			var out api.Schedule
			if err := apiCall(ctx, env, "GET", schedulePath(args[0]), nil, &out); err != nil {
				return err
			}
			return env.Emit(scheduleReport(out))
		},
	}
}

func newSchedulesSetCmd(env *Env) *cobra.Command {
	var every string
	var enable, disable bool

	cmd := &cobra.Command{
		Use:   "set <job-type>",
		Short: "Retune a cadence, or switch a job off",
		Long: "Takes effect without a restart, and re-arms the next run from now —\n" +
			"otherwise lengthening an interval would look like it had not applied\n" +
			"until the old one elapsed.\n\n" +
			"A system schedule can be switched off but not deleted: nothing would\n" +
			"ever recreate it. Switching off the revocation heartbeat widens the\n" +
			"exposure window for every leaked credential in the deployment, which\n" +
			"is a decision you are allowed to make and should make deliberately.",
		Annotations: map[string]string{annotationOperation: "updateSchedule"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if enable && disable {
				return fmt.Errorf("--enable and --disable contradict each other")
			}
			body := map[string]any{}
			if every != "" {
				// Parsed here as well as by the server, so a typo is a local
				// error naming the flag rather than a 400 naming a field.
				if _, err := time.ParseDuration(every); err != nil {
					return fmt.Errorf("--every %q is not a duration: %w", every, err)
				}
				body["spec"] = every
			}
			if enable {
				body["enabled"] = true
			}
			if disable {
				body["enabled"] = false
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to change: pass --every, --enable, or --disable")
			}

			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			var out api.Schedule
			if err := apiCall(ctx, env, "PATCH", schedulePath(args[0]), body, &out); err != nil {
				return err
			}
			return env.Emit(scheduleReport(out))
		},
	}
	cmd.Flags().StringVar(&every, "every", "", "cadence as a duration: 30s, 5m")
	cmd.Flags().BoolVar(&enable, "enable", false, "switch the job on")
	cmd.Flags().BoolVar(&disable, "disable", false, "switch the job off")
	return cmd
}

func newSchedulesRunCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "run <job-type>",
		Short: "Bring a job forward so it runs on the next tick",
		Long: "Does not run the job here. It moves the next run to now and the\n" +
			"scheduler picks it up, through the same claim and the same outcome\n" +
			"recording as any other run — a second execution path would be a\n" +
			"second way for the job to happen, and the two would drift.",
		Annotations: map[string]string{annotationOperation: "runScheduleNow"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			var out api.Schedule
			if err := apiCall(ctx, env, "POST", schedulePath(args[0])+":run", nil, &out); err != nil {
				return err
			}
			return env.Emit(scheduleReport(out))
		},
	}
}

// schedulePath escapes the job type into the path.
//
// A job type is a plain token today. Escaped anyway, because the day one
// contains a slash it would silently address a different route rather than
// fail.
func schedulePath(jobType string) string {
	return "/api/v1/schedules/" + url.PathEscape(jobType)
}

type scheduleListReport api.ScheduleList

func (r scheduleListReport) Table() Table {
	rows := make([][]string, 0, len(r.Schedules))
	for _, s := range r.Schedules {
		rows = append(rows, []string{
			s.JobType, s.Name, s.Spec, scheduleState(s), lastRun(s),
		})
	}
	return Table{
		Columns: []string{"JOB", "NAME", "EVERY", "STATE", "LAST RUN"},
		Rows:    rows,
	}
}

type scheduleReport api.Schedule

func (r scheduleReport) Table() Table {
	s := api.Schedule(r)
	rows := [][]string{
		{"job", s.JobType},
		{"name", s.Name},
		{"every", s.Spec},
		{"state", scheduleState(s)},
		{"system", systemNote(s.System)},
		{"next run", orDash(s.NextRunAt)},
		{"last run", lastRun(s)},
	}
	if s.LastError != "" {
		rows = append(rows, []string{"last error", s.LastError})
	}
	return Table{Columns: []string{"FIELD", "VALUE"}, Rows: rows}
}

// scheduleState reports the last outcome, not merely whether it is switched on.
//
// A job that is enabled and has been failing since Tuesday is the state worth
// seeing, and it reads as healthy in any column that only shows on/off.
func scheduleState(s api.Schedule) string {
	switch {
	case !s.Enabled:
		return "off"
	case s.LastError != "":
		return "failing"
	default:
		return "on"
	}
}

func lastRun(s api.Schedule) string {
	if s.LastRunAt == "" {
		return "never"
	}
	if s.LastDurationMs > 0 {
		return s.LastRunAt + " (" + strconv.Itoa(int(s.LastDurationMs)) + "ms)"
	}
	return s.LastRunAt
}

func systemNote(system bool) string {
	if system {
		return "yes — cannot be deleted"
	}
	return "no"
}

func orDash(in string) string {
	if in == "" {
		return "—"
	}
	return in
}
