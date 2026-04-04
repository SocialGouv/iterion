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
          className="bg-gray-800 border border-gray-500 rounded-lg px-3 py-1.5 text-sm text-white w-64 focus:border-blue-500 focus:outline-none shadow-lg"
          value={searchQuery}
          onChange={(e) => onSearchChange(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Search nodes... (↑↓ navigate, Enter select)"
          autoFocus
        />
        {hasQuery && (
          <span className="text-xs text-gray-400 bg-gray-800/90 border border-gray-600 rounded px-2 py-1 whitespace-nowrap shadow-lg">
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
