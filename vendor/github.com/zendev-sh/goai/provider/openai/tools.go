package openai

import "github.com/zendev-sh/goai/provider"

// Tools provides factory functions for OpenAI provider-defined tools.
// These tools use OpenAI's built-in tool types via the Responses API.
// Matches Vercel AI SDK's openai.tools.
var Tools = struct {
	// WebSearch creates a web search tool definition for the Responses API.
	// The model decides when to search based on the prompt.
	WebSearch func(opts ...WebSearchOption) provider.ToolDefinition

	// CodeInterpreter creates a code interpreter tool definition.
	// Models can write and run Python code in a sandboxed environment.
	CodeInterpreter func(opts ...CodeInterpreterOption) provider.ToolDefinition

	// FileSearch creates a file search tool definition.
	// Models can retrieve information from previously uploaded files via semantic/keyword search.
	FileSearch func(opts ...FileSearchOption) provider.ToolDefinition

	// ImageGeneration creates an image generation tool definition.
	// Models can generate images using GPT Image within a conversation.
	ImageGeneration func(opts ...ImageGenerationOption) provider.ToolDefinition
}{
	WebSearch:       webSearchTool,
	CodeInterpreter: codeInterpreterTool,
	FileSearch:      fileSearchTool,
	ImageGeneration: imageGenerationTool,
}

// ---------------------------------------------------------------------------
// WebSearch
// ---------------------------------------------------------------------------

// WebSearchOption configures the web search tool.
type WebSearchOption func(*webSearchConfig)

type webSearchConfig struct {
	SearchContextSize string             // "low", "medium", "high"
	UserLocation      *WebSearchLocation // optional geolocation
	Filters           *WebSearchFilters  // optional domain filters
	ExternalWebAccess *bool              // optional: true = live content, false = cached
}

// WebSearchLocation provides geographically relevant search results.
type WebSearchLocation struct {
	Type     string // always "approximate"
	Country  string // two-letter ISO country code (e.g., "US", "GB")
	City     string // free text (e.g., "Minneapolis")
	Region   string // free text (e.g., "Minnesota")
	Timezone string // IANA timezone (e.g., "America/Chicago")
}

// WebSearchFilters configures domain filtering for search results.
type WebSearchFilters struct {
	AllowedDomains []string // subdomains of provided domains are allowed
}

// WithSearchContextSize sets the search context size.
// "high" = most comprehensive, highest cost, slower.
// "medium" = balanced (default).
// "low" = least context, lowest cost, fastest.
func WithSearchContextSize(size string) WebSearchOption {
	return func(c *webSearchConfig) { c.SearchContextSize = size }
}

// WithUserLocation provides location for geographically relevant results.
func WithUserLocation(loc WebSearchLocation) WebSearchOption {
	return func(c *webSearchConfig) { c.UserLocation = &loc }
}

// WithSearchFilters configures domain allow-list for search results.
func WithSearchFilters(f WebSearchFilters) WebSearchOption {
	return func(c *webSearchConfig) { c.Filters = &f }
}

// WithExternalWebAccess controls live vs cached content.
// true = fetch live web content (default), false = use cached/indexed results.
func WithExternalWebAccess(enabled bool) WebSearchOption {
	return func(c *webSearchConfig) { c.ExternalWebAccess = &enabled }
}

func webSearchTool(opts ...WebSearchOption) provider.ToolDefinition {
	cfg := &webSearchConfig{}
	for _, o := range opts {
		o(cfg)
	}

	providerOpts := map[string]any{}
	if cfg.SearchContextSize != "" {
		providerOpts["search_context_size"] = cfg.SearchContextSize
	}
	if cfg.UserLocation != nil {
		loc := map[string]any{"type": "approximate"}
		if cfg.UserLocation.Country != "" {
			loc["country"] = cfg.UserLocation.Country
		}
		if cfg.UserLocation.City != "" {
			loc["city"] = cfg.UserLocation.City
		}
		if cfg.UserLocation.Region != "" {
			loc["region"] = cfg.UserLocation.Region
		}
		if cfg.UserLocation.Timezone != "" {
			loc["timezone"] = cfg.UserLocation.Timezone
		}
		providerOpts["user_location"] = loc
	}
	if cfg.Filters != nil && len(cfg.Filters.AllowedDomains) > 0 {
		providerOpts["filters"] = map[string]any{
			"allowed_domains": cfg.Filters.AllowedDomains,
		}
	}
	if cfg.ExternalWebAccess != nil {
		providerOpts["external_web_access"] = *cfg.ExternalWebAccess
	}

	return provider.ToolDefinition{
		Name:                   "web_search",
		ProviderDefinedType:    "web_search",
		ProviderDefinedOptions: providerOpts,
	}
}

// ---------------------------------------------------------------------------
// CodeInterpreter
// ---------------------------------------------------------------------------

// CodeInterpreterOption configures the code interpreter tool.
type CodeInterpreterOption func(*codeInterpreterConfig)

type codeInterpreterConfig struct {
	Container any // string (container ID) or *CodeInterpreterContainer (file IDs)
}

// CodeInterpreterContainer configures a container with uploaded files.
type CodeInterpreterContainer struct {
	FileIDs []string
}

// WithContainerID sets an existing container ID for code interpreter.
func WithContainerID(id string) CodeInterpreterOption {
	return func(c *codeInterpreterConfig) { c.Container = id }
}

// WithContainerFiles sets an auto-provisioned container with uploaded files.
func WithContainerFiles(container *CodeInterpreterContainer) CodeInterpreterOption {
	return func(c *codeInterpreterConfig) { c.Container = container }
}

func codeInterpreterTool(opts ...CodeInterpreterOption) provider.ToolDefinition {
	cfg := &codeInterpreterConfig{}
	for _, o := range opts {
		o(cfg)
	}

	providerOpts := map[string]any{}
	switch v := cfg.Container.(type) {
	case string:
		providerOpts["container"] = v
	case *CodeInterpreterContainer:
		providerOpts["container"] = map[string]any{
			"type":     "auto",
			"file_ids": v.FileIDs,
		}
	default:
		// No container specified → auto with no file IDs.
		providerOpts["container"] = map[string]any{
			"type": "auto",
		}
	}

	return provider.ToolDefinition{
		Name:                   "code_interpreter",
		ProviderDefinedType:    "code_interpreter",
		ProviderDefinedOptions: providerOpts,
	}
}

// ---------------------------------------------------------------------------
// FileSearch
// ---------------------------------------------------------------------------

// FileSearchOption configures the file search tool.
type FileSearchOption func(*fileSearchConfig)

type fileSearchConfig struct {
	VectorStoreIDs []string
	MaxNumResults  int
	Ranking        *FileSearchRanking
	Filters        FileSearchFilter
}

// FileSearchRanking configures ranking options for file search.
type FileSearchRanking struct {
	Ranker         string  // e.g. "auto" or "default_2024_08_21"
	ScoreThreshold float64 // 0-1; closer to 1 = more relevant but fewer results
}

// FileSearchComparisonFilter is a single-field comparison filter.
type FileSearchComparisonFilter struct {
	Key   string // metadata key
	Type  string // "eq", "ne", "gt", "gte", "lt", "lte", "in", "nin"
	Value any    // string, number, bool, or []string
}

// FileSearchCompoundFilter combines multiple filters with AND/OR.
type FileSearchCompoundFilter struct {
	Type    string             // "and" or "or"
	Filters []FileSearchFilter // mix of *FileSearchComparisonFilter and *FileSearchCompoundFilter
}

// FileSearchFilter is implemented by FileSearchComparisonFilter and FileSearchCompoundFilter.
type FileSearchFilter interface {
	fileSearchFilter()
}

func (*FileSearchComparisonFilter) fileSearchFilter() { _ = struct{}{} }
func (*FileSearchCompoundFilter) fileSearchFilter()   { _ = struct{}{} }

// WithVectorStoreIDs sets the vector store IDs to search.
func WithVectorStoreIDs(ids ...string) FileSearchOption {
	return func(c *fileSearchConfig) { c.VectorStoreIDs = ids }
}

// WithMaxNumResults sets the maximum number of search results.
func WithMaxNumResults(n int) FileSearchOption {
	return func(c *fileSearchConfig) { c.MaxNumResults = n }
}

// WithRanking configures the ranking options for file search.
func WithRanking(r FileSearchRanking) FileSearchOption {
	return func(c *fileSearchConfig) { c.Ranking = &r }
}

// WithFileSearchFilters sets the metadata filter for file search.
func WithFileSearchFilters(f FileSearchFilter) FileSearchOption {
	return func(c *fileSearchConfig) { c.Filters = f }
}

func fileSearchTool(opts ...FileSearchOption) provider.ToolDefinition {
	cfg := &fileSearchConfig{}
	for _, o := range opts {
		o(cfg)
	}

	providerOpts := map[string]any{}
	if len(cfg.VectorStoreIDs) > 0 {
		providerOpts["vector_store_ids"] = cfg.VectorStoreIDs
	}
	if cfg.MaxNumResults > 0 {
		providerOpts["max_num_results"] = cfg.MaxNumResults
	}
	if cfg.Ranking != nil {
		ranking := map[string]any{}
		if cfg.Ranking.Ranker != "" {
			ranking["ranker"] = cfg.Ranking.Ranker
		}
		if cfg.Ranking.ScoreThreshold > 0 {
			ranking["score_threshold"] = cfg.Ranking.ScoreThreshold
		}
		providerOpts["ranking_options"] = ranking
	}
	if cfg.Filters != nil {
		providerOpts["filters"] = serializeFilter(cfg.Filters)
	}

	return provider.ToolDefinition{
		Name:                   "file_search",
		ProviderDefinedType:    "file_search",
		ProviderDefinedOptions: providerOpts,
	}
}

func serializeFilter(f FileSearchFilter) any {
	switch v := f.(type) {
	case *FileSearchComparisonFilter:
		return map[string]any{
			"key":   v.Key,
			"type":  v.Type,
			"value": v.Value,
		}
	case *FileSearchCompoundFilter:
		filters := make([]any, len(v.Filters))
		for i, sub := range v.Filters {
			filters[i] = serializeFilter(sub)
		}
		return map[string]any{
			"type":    v.Type,
			"filters": filters,
		}
	default:
		return nil // unreachable - FileSearchFilter is sealed
	}
}

// ---------------------------------------------------------------------------
// ImageGeneration
// ---------------------------------------------------------------------------

// ImageGenerationOption configures the image generation tool.
type ImageGenerationOption func(*imageGenerationConfig)

type imageGenerationConfig struct {
	Background       string                   // "auto", "opaque", "transparent"
	InputFidelity    string                   // "low", "high"
	InputImageMask   *ImageGenerationMask     // optional mask for inpainting
	Model            string                   // e.g. "gpt-image-1"
	Moderation       string                   // "auto"
	OutputCompression *int                    // 0-100
	OutputFormat     string                   // "png", "jpeg", "webp"
	PartialImages    *int                     // 0-3, for streaming
	Quality          string                   // "auto", "low", "medium", "high"
	Size             string                   // "auto", "1024x1024", "1024x1536", "1536x1024"
}

// ImageGenerationMask provides a mask for inpainting.
type ImageGenerationMask struct {
	FileID   string
	ImageURL string
}

// WithBackground sets the background type for the generated image.
func WithBackground(bg string) ImageGenerationOption {
	return func(c *imageGenerationConfig) { c.Background = bg }
}

// WithInputFidelity sets input fidelity ("low" or "high").
func WithInputFidelity(fidelity string) ImageGenerationOption {
	return func(c *imageGenerationConfig) { c.InputFidelity = fidelity }
}

// WithInputImageMask sets the inpainting mask.
func WithInputImageMask(mask ImageGenerationMask) ImageGenerationOption {
	return func(c *imageGenerationConfig) { c.InputImageMask = &mask }
}

// WithImageModel sets the image generation model (default: "gpt-image-1").
func WithImageModel(model string) ImageGenerationOption {
	return func(c *imageGenerationConfig) { c.Model = model }
}

// WithModeration sets the moderation level (default: "auto").
func WithModeration(mod string) ImageGenerationOption {
	return func(c *imageGenerationConfig) { c.Moderation = mod }
}

// WithOutputCompression sets the output compression level (0-100).
func WithOutputCompression(level int) ImageGenerationOption {
	return func(c *imageGenerationConfig) { c.OutputCompression = &level }
}

// WithOutputFormat sets the output image format ("png", "jpeg", "webp").
func WithOutputFormat(format string) ImageGenerationOption {
	return func(c *imageGenerationConfig) { c.OutputFormat = format }
}

// WithPartialImages sets the number of partial images in streaming mode (0-3).
func WithPartialImages(n int) ImageGenerationOption {
	return func(c *imageGenerationConfig) { c.PartialImages = &n }
}

// WithImageQuality sets the image quality ("auto", "low", "medium", "high").
func WithImageQuality(quality string) ImageGenerationOption {
	return func(c *imageGenerationConfig) { c.Quality = quality }
}

// WithImageSize sets the image size ("auto", "1024x1024", "1024x1536", "1536x1024").
func WithImageSize(size string) ImageGenerationOption {
	return func(c *imageGenerationConfig) { c.Size = size }
}

func imageGenerationTool(opts ...ImageGenerationOption) provider.ToolDefinition {
	cfg := &imageGenerationConfig{}
	for _, o := range opts {
		o(cfg)
	}

	providerOpts := map[string]any{}
	if cfg.Background != "" {
		providerOpts["background"] = cfg.Background
	}
	if cfg.InputFidelity != "" {
		providerOpts["input_fidelity"] = cfg.InputFidelity
	}
	if cfg.InputImageMask != nil {
		mask := map[string]any{}
		if cfg.InputImageMask.FileID != "" {
			mask["file_id"] = cfg.InputImageMask.FileID
		}
		if cfg.InputImageMask.ImageURL != "" {
			mask["image_url"] = cfg.InputImageMask.ImageURL
		}
		providerOpts["input_image_mask"] = mask
	}
	if cfg.Model != "" {
		providerOpts["model"] = cfg.Model
	}
	if cfg.Moderation != "" {
		providerOpts["moderation"] = cfg.Moderation
	}
	if cfg.OutputCompression != nil {
		providerOpts["output_compression"] = *cfg.OutputCompression
	}
	if cfg.OutputFormat != "" {
		providerOpts["output_format"] = cfg.OutputFormat
	}
	if cfg.PartialImages != nil {
		providerOpts["partial_images"] = *cfg.PartialImages
	}
	if cfg.Quality != "" {
		providerOpts["quality"] = cfg.Quality
	}
	if cfg.Size != "" {
		providerOpts["size"] = cfg.Size
	}

	return provider.ToolDefinition{
		Name:                   "image_generation",
		ProviderDefinedType:    "image_generation",
		ProviderDefinedOptions: providerOpts,
	}
}
