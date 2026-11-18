"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * Top bar 'new version available' notice: check once on full page load, and show it next to the version number if there is an update,
 * Click to jump straight to the 'Version & Updates' card on the system settings page.
 *
 * The backend caches GitHub query results for 30 minutes, so checking once on every mount here is safe
 * — the unauthenticated GitHub API only allows 60 requests/hour/IP; without that cache, opening a few tabs
 * would exhaust the quota, and then when you actually want to update, the check won't work.
 *
 * Any query failure is silent: the top bar isn't the place for errors; users who go to the settings page and click 'Check for updates' will see the reason.
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update already includes the 'version numbers are comparable' check, so dev builds won't show this notice.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // Silent: no network / GitHub rate limiting shouldn't pop an error in the top bar.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`New version found ${latest}, click to update`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* Breathing dot: the top bar has many elements, plain text is easy to miss, the animation makes it visible at a glance. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">New version {latest}</span>
      <span className="sm:hidden">New version</span>
    </Link>
  );
}
