import {
  createHashHistory,
  createRootRoute,
  createRoute,
  createRouter,
  lazyRouteComponent,
} from "@tanstack/react-router";
import { App, NotFound, RouteError } from "./App";

const rootRoute = createRootRoute({
  component: App,
  errorComponent: RouteError,
  notFoundComponent: NotFound,
});

const overviewRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: lazyRouteComponent(() => import("./routes/OverviewRoute"), "OverviewRoute"),
});

const reposRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/repos",
  component: lazyRouteComponent(() => import("./routes/ReposRoute"), "ReposRoute"),
});

const botsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/bots",
  component: lazyRouteComponent(() => import("./routes/BotsRoute"), "BotsRoute"),
});

const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/setup",
  component: lazyRouteComponent(() => import("./routes/SetupRoute"), "SetupRoute"),
});

const settingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings",
  component: lazyRouteComponent(() => import("./routes/SettingsRoute"), "SettingsRoute"),
});

export const prRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/pr/$owner/$name/$pr",
  params: {
    parse: ({ name, owner, pr }) => {
      const number = Number(pr);
      return Number.isInteger(number) && number > 0 ? { name, owner, pr: number } : false;
    },
    stringify: ({ name, owner, pr }) => ({
      name,
      owner,
      pr: String(pr),
    }),
  },
  component: lazyRouteComponent(() => import("./routes/PRRoute"), "PRRoute"),
});

const routeTree = rootRoute.addChildren([
  overviewRoute,
  reposRoute,
  botsRoute,
  setupRoute,
  settingsRoute,
  prRoute,
]);

export const router = createRouter({
  routeTree,
  history: createHashHistory(),
  defaultPreload: "intent",
  defaultPreloadDelay: 120,
  defaultPreloadStaleTime: 3000,
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
