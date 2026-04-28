import { forwardRef } from "react";

interface Props {
  searchQuery: string;
  onSearchChange: (value: string) => void;
  onKeyDown: (e: React.KeyboardEvent) => void;
  matchCount: number;
  currentIndex: number;
}

const SearchOverlay = forwardRef<HTMLInputElement, Props>(
  ({ searchQuery, onSearchChange, onKeyDown, matchCount, currentIndex }, ref) => {
    const hasQuery = searchQuery.trim().length > 0;

    return (
      <div className="absolute top-2 left-1/2 -translate-x-1/2 z-50 flex items-center gap-2">
        <input
          ref={ref}
          className="bg-surface-1 border border-border-strong rounded-lg px-3 py-1.5 text-sm text-fg-default w-64 focus:border-accent focus:outline-none shadow-lg"
          value={searchQuery}
          onChange={(e) => onSearchChange(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Search nodes... (↑↓ navigate, Enter select)"
          autoFocus
        />
        {hasQuery && (
          <span className="text-xs text-fg-subtle bg-surface-1/90 border border-border-strong rounded px-2 py-1 whitespace-nowrap shadow-lg">
            {matchCount > 0
              ? `${currentIndex + 1} / ${matchCount}`
              : "No matches"}
          </span>
        )}
      </div>
    );
  },
);

SearchOverlay.displayName = "SearchOverlay";

export default SearchOverlay;
