import { useCallback, useEffect } from "react";
import { useUIStore } from "@/store/ui";

export function useFullscreen() {
  const setBrowserFullscreen = useUIStore((s) => s.setBrowserFullscreen);

  useEffect(() => {
    const handler = () => setBrowserFullscreen(!!window.document.fullscreenElement);
    window.document.addEventListener("fullscreenchange", handler);
    return () => window.document.removeEventListener("fullscreenchange", handler);
  }, [setBrowserFullscreen]);

  const toggleFullscreen = useCallback(() => {
    if (window.document.fullscreenElement) {
      window.document.exitFullscreen();
    } else {
      window.document.documentElement.requestFullscreen();
    }
  }, []);

  return { toggleFullscreen };
}
