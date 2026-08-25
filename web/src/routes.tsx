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
import { LoginScreen } from "./screens/LoginScreen.tsx";
import { LogoutScreen } from "./screens/LogoutScreen.tsx";

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
import { SchedulesScreen, ScheduleScreen } from "./screens/SchedulesScreen.tsx";
import { KeysScreen } from "./screens/KeysScreen.tsx";
import { GatewayScreen } from "./screens/GatewayScreen.tsx";
import { BackendsScreen } from "./screens/BackendsScreen.tsx";
import { TenantsScreen } from "./screens/TenantsScreen.tsx";
import { UsersScreen } from "./screens/UsersScreen.tsx";
import { AllUsersScreen } from "./screens/AllUsersScreen.tsx";
import { UserScreen } from "./screens/UserScreen.tsx";
import { GrantsScreen } from "./screens/GrantsScreen.tsx";
import { APIKeysScreen } from "./screens/APIKeysScreen.tsx";
import { RolesScreen } from "./screens/RolesScreen.tsx";
import { SessionScreen } from "./screens/SessionScreen.tsx";
import { RevocationsScreen } from "./screens/RevocationsScreen.tsx";
import { InspectorScreen } from "./screens/InspectorScreen.tsx";
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
    | "Overview"
    | "Tenancy"
    | "Registry"
    | "Catalog"
    | "Data plane"
    | "Control plane";
}

export const ROUTES: RouteDef[] = [
  // Sign-in and sign-out. `/login` is also rendered by App.tsx *outside* the
  // auth guard — signing in cannot require being signed in — and appears here
  // so `make parity` can see that the operation has a route. Listing it twice
  // is harmless: the guard's route never matches for a signed-out visitor.
  {
    path: "/login",
    operation: "login",
    component: LoginScreen,
  },
  {
    path: "/logout",
    operation: "logout",
    component: LogoutScreen,
  },
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

  // "Catalog", not "Snapshots" (ADR 0025). The catalog rebuilds itself, so a
  // snapshot is an implementation detail of how it gets there — signed,
  // versioned, and not something anyone has to hold in their head to use this.
  {
    path: "/snapshots",
    operation: "getCurrentSnapshot",
    component: SnapshotScreen,
    nav: "What is serving",
    section: "Catalog",
  },
  {
    path: "/snapshots/build",
    operation: "buildSnapshot",
    component: SnapshotBuildScreen,
    nav: "Rebuild now",
    section: "Catalog",
  },
  // File tools, and that is where they belong. These read a snapshot file
  // somebody hands them — they are how you answer "what is in this artifact",
  // not part of running the gateway.
  // Timed work (ADR 0026). Under Control plane because that is the process
  // that runs it — the data plane's own timers stay in its config, or it would
  // need this database to keep serving through a control-plane outage.
  {
    path: "/schedules",
    operation: "listSchedules",
    component: SchedulesScreen,
    nav: "Schedules",
    section: "Control plane",
  },
  {
    path: "/schedules/:jobType",
    operation: "getSchedule",
    component: ScheduleScreen,
  },
  {
    path: "/schedules/:jobType/edit",
    operation: "updateSchedule",
    component: ScheduleScreen,
  },
  {
    path: "/schedules/:jobType/run",
    operation: "runScheduleNow",
    component: ScheduleScreen,
  },
  {
    path: "/snapshots/inspect",
    operation: "inspectSnapshot",
    component: SnapshotInspectScreen,
    nav: "Inspect a file",
    section: "Control plane",
  },
  {
    path: "/snapshots/verify",
    operation: "verifySnapshot",
    component: SnapshotVerifyScreen,
    nav: "Verify a file",
    section: "Control plane",
  },
  {
    path: "/snapshots/keys",
    operation: "generateSigningKey",
    component: KeysScreen,
    nav: "Signing keys",
    section: "Control plane",
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
  // Tenancy. Several operations share a screen and differ by path: the create
  // form, the delete confirmation, and the grant editor live on the page that
  // lists what they act on, because a confirmation which does not show what it
  // is about is not a confirmation. The route is the deep link into that mode.
  {
    path: "/session",
    operation: "getSession",
    component: SessionScreen,
    nav: "Your credential",
    section: "Tenancy",
  },
  {
    path: "/gateway/inspector",
    component: InspectorScreen,
    nav: "Inspector",
    section: "Data plane",
  },
  {
    path: "/gateway/revocations",
    operation: "getRevocations",
    component: RevocationsScreen,
    nav: "Revocations",
    section: "Data plane",
  },
  {
    path: "/tenants",
    operation: "listTenants",
    component: TenantsScreen,
    nav: "Tenants",
    section: "Tenancy",
  },
  {
    path: "/tenants/new",
    operation: "createTenant",
    component: TenantsScreen,
  },
  {
    path: "/tenants/:tenantId/delete",
    operation: "deleteTenant",
    component: TenantsScreen,
  },
  {
    path: "/users",
    operation: "listAllUsers",
    component: AllUsersScreen,
    nav: "Users",
    section: "Tenancy",
  },
  {
    path: "/users/new",
    operation: "createUser",
    component: AllUsersScreen,
  },
  {
    path: "/tenants/:tenantId/users",
    operation: "listUsers",
    component: UsersScreen,
  },
  {
    path: "/users/:userId",
    operation: "getUser",
    component: UserScreen,
  },
  {
    path: "/users/:userId/edit",
    operation: "updateUser",
    component: UserScreen,
  },
  {
    path: "/users/:userId/delete",
    operation: "deleteUser",
    component: UserScreen,
  },
  {
    path: "/users/:userId/grants",
    operation: "listGrants",
    component: GrantsScreen,
  },
  {
    path: "/users/:userId/grants/edit",
    operation: "putGrants",
    component: GrantsScreen,
  },
  {
    path: "/users/:userId/keys",
    operation: "listAPIKeys",
    component: APIKeysScreen,
  },
  {
    path: "/users/:userId/keys/new",
    operation: "mintAPIKey",
    component: APIKeysScreen,
  },
  {
    path: "/users/:userId/keys/:keyId/revoke",
    operation: "revokeAPIKey",
    component: APIKeysScreen,
  },
  {
    path: "/roles",
    operation: "listRoles",
    component: RolesScreen,
    nav: "Roles",
    section: "Tenancy",
  },
  {
    path: "/gateway/catalog",
    operation: "getCatalog",
    component: CatalogScreen,
    nav: "Inspect a principal",
    section: "Data plane",
  },
  {
    path: "/gateway/playground",
    operation: "callTool",
    component: PlaygroundScreen,
    nav: "Call a tool",
    section: "Data plane",
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
  "Tenancy",
  "Registry",
  "Catalog",
  "Data plane",
  "Control plane",
] as const;
