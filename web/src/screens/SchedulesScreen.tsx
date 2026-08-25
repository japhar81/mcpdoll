import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import {
  listSchedules,
  runScheduleNow,
  updateSchedule,
} from "../lib/api.ts";
import { ErrorBlock, Screen, Table } from "../components/Screen.tsx";
import type { Schedule, ScheduleList } from "../lib/types.ts";

// Everything the platform does without being asked (ADR 0026).
//
// One screen rather than one per operation: "what runs on its own, how often,
// and did it work" is a single question, and answering it across four screens
// would be the thing this replaced.
export function SchedulesScreen() {
  const q = useQuery({
    queryKey: ["schedules"],
    queryFn: listSchedules,
    // These rows move on their own — that is what they are for.
    refetchInterval: 5_000,
  });

  return (
    <Screen title="Schedules" isLoading={q.isLoading} error={q.error}>
      <p className="muted">
        Timed work the control plane does on its own. Cadences are editable and
        take effect without a restart; system schedules can be switched off but
        not deleted, because nothing would recreate them.
      </p>
      {q.data && (
        <Table
          columns={["Job", "Every", "Last run", "State", ""]}
          rows={q.data.schedules.map((s) => [
            <>
              {s.name}
              <br />
              <code className="muted">{s.job_type}</code>
            </>,
            <Cadence schedule={s} />,
            <LastRun schedule={s} />,
            <State schedule={s} />,
            <RunNow schedule={s} />,
          ])}
        />
      )}

      {q.data && <DataPlaneTimers list={q.data} />}
    </Screen>
  );
}

// The data plane's cadences, shown and not editable.
//
// Here because a page titled "Schedules" that listed only half the platform's
// timed work would leave somebody concluding nothing else runs. Read-only
// because these live in the data plane's own config: a probe cadence stored in
// the control plane's database would stop the data plane noticing an unhealthy
// backend during exactly the control-plane outage the split exists to survive.
function DataPlaneTimers(props: { list: ScheduleList }) {
  const { data_plane_timers, data_plane_source, data_plane_error } = props.list;

  return (
    <>
      <h2>Data plane</h2>
      {data_plane_error ? (
        <div className="note warn">
          Could not read the data plane&apos;s timers, so this list is only the
          control plane&apos;s: {data_plane_error}
        </div>
      ) : (
        <p className="muted">
          The data plane runs these itself and they are not editable here.
          They live in {data_plane_source ?? "its config file"} on purpose — a
          probe cadence stored in the control plane&apos;s database would stop
          the data plane noticing an unhealthy backend during exactly the
          control-plane outage it is built to survive.
        </p>
      )}
      {data_plane_timers && data_plane_timers.length > 0 && (
        <Table
          columns={["Timer", "Every"]}
          rows={data_plane_timers.map((t) => [
            <>
              {t.name}
              {t.description && (
                <>
                  <br />
                  <span className="muted">{t.description}</span>
                </>
              )}
            </>,
            t.every,
          ])}
        />
      )}
    </>
  );
}

function Cadence(props: { schedule: Schedule }) {
  const client = useQueryClient();
  const [spec, setSpec] = useState(props.schedule.spec);
  const m = useMutation({
    mutationFn: (next: string) =>
      updateSchedule(props.schedule.job_type, { spec: next }),
    onSuccess: () => void client.invalidateQueries({ queryKey: ["schedules"] }),
  });

  return (
    <>
      <input
        className="narrow"
        value={spec}
        onChange={(e) => setSpec(e.target.value)}
        onBlur={() => spec !== props.schedule.spec && m.mutate(spec)}
        aria-label={`cadence for ${props.schedule.name}`}
      />
      {m.error != null && <ErrorBlock error={m.error} />}
    </>
  );
}

function LastRun(props: { schedule: Schedule }) {
  const { last_run_at, last_duration_ms } = props.schedule;
  if (!last_run_at) {
    return <span className="muted">never</span>;
  }
  return (
    <>
      {new Date(last_run_at).toLocaleTimeString()}
      {last_duration_ms ? (
        <span className="muted"> · {last_duration_ms}ms</span>
      ) : null}
    </>
  );
}

// The last outcome, not just on/off. A job that is enabled and has been failing
// since Tuesday is the state worth seeing, and it reads as healthy anywhere
// that only shows whether it is switched on.
function State(props: { schedule: Schedule }) {
  const client = useQueryClient();
  const m = useMutation({
    mutationFn: (enabled: boolean) =>
      updateSchedule(props.schedule.job_type, { enabled }),
    onSuccess: () => void client.invalidateQueries({ queryKey: ["schedules"] }),
  });

  if (props.schedule.last_error) {
    return <span className="pill bad">failing: {props.schedule.last_error}</span>;
  }
  return (
    <label className="inline">
      <input
        type="checkbox"
        checked={props.schedule.enabled}
        onChange={(e) => m.mutate(e.target.checked)}
      />
      {props.schedule.enabled ? "on" : "off"}
    </label>
  );
}

function RunNow(props: { schedule: Schedule }) {
  const client = useQueryClient();
  const m = useMutation({
    mutationFn: () => runScheduleNow(props.schedule.job_type),
    onSuccess: () => void client.invalidateQueries({ queryKey: ["schedules"] }),
  });
  return (
    <button
      disabled={!props.schedule.enabled || m.isPending}
      onClick={() => m.mutate()}
    >
      Run now
    </button>
  );
}

// The detail route parity requires. A schedule is four fields and a history of
// one, so a page per job would be a page with less on it than the table row it
// came from — this sends you back to the one place that answers the question.
export function ScheduleScreen() {
  const { jobType } = useParams();
  const q = useQuery({ queryKey: ["schedules"], queryFn: listSchedules });
  const found = q.data?.schedules.find((s) => s.job_type === jobType);

  return (
    <Screen title={found?.name ?? jobType ?? "Schedule"} isLoading={q.isLoading} error={q.error}>
      {found && (
        <Table
          columns={["Field", "Value"]}
          rows={[
            ["Job", <code>{found.job_type}</code>],
            ["Every", found.spec],
            ["Enabled", found.enabled ? "yes" : "no"],
            ["System", found.system ? "yes — cannot be deleted" : "no"],
            ["Next run", found.next_run_at ?? "—"],
            ["Last run", found.last_run_at ?? "never"],
            ["Last outcome", found.last_error || "ok"],
          ]}
        />
      )}
    </Screen>
  );
}
