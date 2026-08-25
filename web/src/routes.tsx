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
import { KeysScreen } from "./screens/KeysScreen.tsx";
import { GatewayScreen } from "./screens/GatewayScreen.tsx";
import { BackendsScreen } from "./screens/BackendsScreen.tsx";
import { TenantsScreen } from "./screens/TenantsScreen.tsx";
import { UsersScreen } from "./screens/UsersScreen.tsx";
import { UserScreen } from "./screens/UserScreen.tsx";
import { GrantsScreen } from "./screens/GrantsScreen.tsx";
import { APIKeysScreen } from "./screens/APIKeysScreen.tsx";
import { RolesScreen } from "./screens/RolesScreen.tsx";
import { SessionScreen } from "./screens/SessionScreen.tsx";
import { RevocationsScreen } from "./screens/RevocationsScreen.tsx";
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
    | "Snapshots"
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
    path: "/tenants/:tenantId/users",
    operation: "listUsers",
    component: UsersScreen,
  },
  {
    path: "/tenants/:tenantId/users/new",
    operation: "createUser",
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
  "Snapshots",
  "Data plane",
  "Control plane",
] as const;
