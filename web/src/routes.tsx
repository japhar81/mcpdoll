/**
 * The console's route table — one entry per API operation.
 *
 * This is the source `scripts/gen-routes.mjs` reads to emit `routes.gen.ts`,
 * which `make parity` checks against api/openapi.yaml. Adding a screen means
 * adding a row here; forgetting to means the build fails with the operation
 * named.
 *
 * `operation` is not decorative. It is the join key between this table, the
 * OpenAPI document, and the CLI's command annotations — the three surfaces the
 * first law says every feature must reach.
 */
import type { ComponentType } from "react";

import { OverviewScreen } from "./screens/OverviewScreen.tsx";

import { HealthScreen } from "./screens/HealthScreen.tsx";
import { HooksScreen } from "./screens/HooksScreen.tsx";
import { RegistryScreen } from "./screens/RegistryScreen.tsx";
import { RegistryValidateScreen } from "./screens/RegistryValidateScreen.tsx";
import { ServersScreen } from "./screens/ServersScreen.tsx";
import { ServerDetailScreen } from "./screens/ServerDetailScreen.tsx";
import { PluginsScreen } from "./screens/PluginsScreen.tsx";
import { SnapshotScreen } from "./screens/SnapshotScreen.tsx";
import { SnapshotInspectScreen } from "./screens/SnapshotInspectScreen.tsx";
import { SnapshotBuildScreen } from "./screens/SnapshotBuildScreen.tsx";
import { SnapshotVerifyScreen } from "./screens/SnapshotVerifyScreen.tsx";
import { KeysScreen } from "./screens/KeysScreen.tsx";
import { GatewayScreen } from "./screens/GatewayScreen.tsx";
import { BackendsScreen } from "./screens/BackendsScreen.tsx";
import { AudiencesScreen } from "./screens/AudiencesScreen.tsx";
import { CatalogScreen } from "./screens/CatalogScreen.tsx";
import { PlaygroundScreen } from "./screens/PlaygroundScreen.tsx";

export interface RouteDef {
  /** React Router path. Params use `:name`, matching the OpenAPI declaration. */
  path: string;
  /** The operationId this screen is the UI for. Absent on screens that
   *  compose existing operations rather than adding one — the overview and
   *  the login page. */
  operation?: string;
  component: ComponentType;
  /** Sidebar label. Absent means reachable only by navigation from elsewhere. */
  nav?: string;
  /** Sidebar grouping. Named after the thing, not the feature: "Data plane"
   *  and "Control plane" are what a reader needs to tell apart. */
  section?:
    "Overview" | "Registry" | "Snapshots" | "Data plane" | "Control plane";
}

export const ROUTES: RouteDef[] = [
  // The landing page. It binds no operation — it composes several — so it does
  // not appear in the generated manifest and does not affect `make parity`.
  {
    path: "/overview",
    component: OverviewScreen,
    nav: "Overview",
    section: "Overview",
  },
  {
    path: "/registry",
    operation: "getRegistry",
    component: RegistryScreen,
    nav: "Registry",
    section: "Registry",
  },
  {
    path: "/registry/servers",
    operation: "listServers",
    component: ServersScreen,
    nav: "Backends",
    section: "Registry",
  },
  {
    path: "/registry/servers/:serverId",
    operation: "getServer",
    component: ServerDetailScreen,
  },
  {
    path: "/registry/validate",
    operation: "validateRegistry",
    component: RegistryValidateScreen,
    nav: "Validate",
    section: "Registry",
  },
  {
    path: "/plugins",
    operation: "listPlugins",
    component: PluginsScreen,
    nav: "Plugins",
    section: "Registry",
  },
  {
    path: "/plugins/hooks",
    operation: "listHooks",
    component: HooksScreen,
    nav: "Hooks",
    section: "Registry",
  },

  {
    path: "/snapshots",
    operation: "getCurrentSnapshot",
    component: SnapshotScreen,
    nav: "Current",
    section: "Snapshots",
  },
  {
    path: "/snapshots/build",
    operation: "buildSnapshot",
    component: SnapshotBuildScreen,
    nav: "Build",
    section: "Snapshots",
  },
  {
    path: "/snapshots/inspect",
    operation: "inspectSnapshot",
    component: SnapshotInspectScreen,
    nav: "Inspect",
    section: "Snapshots",
  },
  {
    path: "/snapshots/verify",
    operation: "verifySnapshot",
    component: SnapshotVerifyScreen,
    nav: "Verify",
    section: "Snapshots",
  },
  {
    path: "/snapshots/keys",
    operation: "generateSigningKey",
    component: KeysScreen,
    nav: "Signing keys",
    section: "Snapshots",
  },

  {
    path: "/gateway",
    operation: "getGatewayStatus",
    component: GatewayScreen,
    nav: "Status",
    section: "Data plane",
  },
  {
    path: "/gateway/backends",
    operation: "listBackends",
    component: BackendsScreen,
    nav: "Backend health",
    section: "Data plane",
  },
  {
    path: "/gateway/audiences",
    operation: "listAudiences",
    component: AudiencesScreen,
    nav: "Audiences",
    section: "Data plane",
  },
  {
    path: "/gateway/audiences/:slug/catalog",
    operation: "getAudienceCatalog",
    component: CatalogScreen,
  },
  {
    path: "/gateway/audiences/:slug/playground",
    operation: "callTool",
    component: PlaygroundScreen,
  },

  {
    path: "/system/health",
    operation: "getHealth",
    component: HealthScreen,
    nav: "Health",
    section: "Control plane",
  },
];

export const SECTIONS = [
  "Overview",
  "Registry",
  "Snapshots",
  "Data plane",
  "Control plane",
] as const;
