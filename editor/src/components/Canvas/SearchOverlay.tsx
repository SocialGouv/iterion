import { forwardRef } from "react";

interface Props {
  searchQuery: string;
  onSearchChange: (value: string) => void;
  onKeyDown: (e: React.KeyboardEvent) => void;
}

const SearchOverlay = forwardRef<HTMLInputElement, Props>(
  ({ searchQuery, onSearchChange, onKeyDown }, ref) => {
    return (
      <div className="absolute top-2 left-1/2 -translate-x-1/2 z-50">
        <input
          ref={ref}
          className="bg-gray-800 border border-gray-500 rounded-lg px-3 py-1.5 text-sm text-white w-64 focus:border-blue-500 focus:outline-none shadow-lg"
          value={searchQuery}
          onChange={(e) => onSearchChange(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Search nodes... (Enter to select, Esc to close)"
          autoFocus
        />
      </div>
    );
  },
);

SearchOverlay.displayName = "SearchOverlay";

export default SearchOverlay;
