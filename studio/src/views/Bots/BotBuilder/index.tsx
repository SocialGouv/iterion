import { Link } from "wouter";

import { Button, Card } from "@/components/ui";

/**
 * BotBuilderView — /bots/new. Minimal stub so the route (and every
 * "New bot" affordance pointing at it) compiles; the guided builder
 * (template gallery via listBotTemplates + createBot) lands in a
 * follow-up task on this branch.
 */
export default function BotBuilderView() {
  return (
    <div className="mx-auto w-full max-w-2xl p-4">
      <Card>
        <h1 className="text-sm font-semibold text-fg-default">New bot</h1>
        <p className="mt-1 text-xs text-fg-muted">
          The guided bot builder is coming in this branch. Until it lands, scaffold a bot with{" "}
          <code className="font-mono text-fg-default">iterion bundle init</code>, or import an
          existing bundle from the Bots gallery.
        </p>
        <div className="mt-3">
          <Link href="/bots">
            <Button variant="secondary" size="sm">
              Back to Bots
            </Button>
          </Link>
        </div>
      </Card>
    </div>
  );
}
