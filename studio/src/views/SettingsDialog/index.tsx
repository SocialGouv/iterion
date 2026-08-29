import { Dialog, Tabs } from "@/components/ui";

import AppearanceTab from "./AppearanceTab";
import BackendsTab from "./BackendsTab";
import AssistantTab from "./AssistantTab";
import ApiKeysTab from "./ApiKeysTab";
import ProjectsTab from "./ProjectsTab";
import UpdatesTab from "./UpdatesTab";
import AboutTab from "./AboutTab";
import StorageTab from "./StorageTab";

interface Props {
  open: boolean;
  onClose: () => void;
  tab: string;
  onTabChange: (tab: string) => void;
  /** When true, surface tabs that depend on the desktop bridge
   *  (api keys via OS keychain, projects, updates, native About). The
   *  Appearance, Backends, Storage tabs work everywhere. */
  desktopFeatures: boolean;
}

export default function SettingsDialog({
  open,
  onClose,
  tab,
  onTabChange,
  desktopFeatures,
}: Props) {
  const tabItems = [
    { value: "appearance", label: "Appearance" },
    { value: "backends", label: "Backends" },
    { value: "assistant", label: "Assistant" },
    ...(desktopFeatures ? [{ value: "api-keys", label: "API keys" }] : []),
    ...(desktopFeatures ? [{ value: "projects", label: "Projects" }] : []),
    { value: "storage", label: "Storage" },
    ...(desktopFeatures ? [{ value: "updates", label: "Updates" }] : []),
    { value: "about", label: desktopFeatures ? "About" : "About" },
  ];
  const panels: Record<string, React.ReactNode> = {
    appearance: <AppearanceTab />,
    backends: <BackendsTab />,
    assistant: <AssistantTab />,
    storage: <StorageTab />,
    about: <AboutTab desktopFeatures={desktopFeatures} />,
  };
  if (desktopFeatures) {
    panels["api-keys"] = <ApiKeysTab />;
    panels.projects = <ProjectsTab />;
    panels.updates = <UpdatesTab />;
  }
  // Guard against stale tab state pointing at a desktop-only tab when
  // the dialog is opened in web mode.
  const safeTab = panels[tab] ? tab : "appearance";
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => !o && onClose()}
      title="Settings"
      widthClass="max-w-3xl"
    >
      <Tabs
        value={safeTab}
        onValueChange={onTabChange}
        items={tabItems}
        panels={panels}
        className="min-h-[420px]"
      />
    </Dialog>
  );
}
