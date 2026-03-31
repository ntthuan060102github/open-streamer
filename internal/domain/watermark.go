package domain

// WatermarkType determines whether the overlay is text or an image.
type WatermarkType string

const (
	WatermarkTypeText  WatermarkType = "text"
	WatermarkTypeImage WatermarkType = "image"
)

// WatermarkPosition controls where the overlay is placed in the frame.
type WatermarkPosition string

const (
	WatermarkTopLeft     WatermarkPosition = "top_left"
	WatermarkTopRight    WatermarkPosition = "top_right"
	WatermarkBottomLeft  WatermarkPosition = "bottom_left"
	WatermarkBottomRight WatermarkPosition = "bottom_right"
	WatermarkCenter      WatermarkPosition = "center"
)

// WatermarkConfig defines an overlay applied to the video before encoding.
// Applied via FFmpeg drawtext (text) or overlay (image) filter.
type WatermarkConfig struct {
	Enabled bool          `json:"enabled"`
	Type    WatermarkType `json:"type"`

	// --- Text overlay ---

	// Text is the string to render. Supports strftime directives for live timestamps.
	// E.g. "LIVE %{localtime:%H:%M:%S}"
	Text string `json:"text"`

	// FontFile is the path to a .ttf/.otf font file.
	// "" = FFmpeg default font.
	FontFile string `json:"font_file"`

	// FontSize in pixels. Default: 24.
	FontSize int `json:"font_size"`

	// FontColor in FFmpeg color syntax. E.g. "white", "#FFFFFF", "white@0.8".
	FontColor string `json:"font_color"`

	// --- Image overlay ---

	// ImagePath is the path to the watermark image (PNG with alpha recommended).
	ImagePath string `json:"image_path"`

	// --- Common ---

	// Opacity controls transparency: 0.0 = fully transparent, 1.0 = fully opaque.
	Opacity float64 `json:"opacity"`

	// Position is the corner/center anchor for the watermark.
	Position WatermarkPosition `json:"position"`

	// OffsetX and OffsetY are pixel offsets from the chosen position edge.
	OffsetX int `json:"offset_x"`
	OffsetY int `json:"offset_y"`
}

// ThumbnailConfig controls periodic screenshot generation for stream preview.
// Thumbnails are written as JPEG files alongside HLS segments.
type ThumbnailConfig struct {
	Enabled bool `json:"enabled"`

	// IntervalSec generates one thumbnail every N seconds.
	IntervalSec int `json:"interval_sec"`

	// Width and Height of the output thumbnail in pixels.
	// 0 = match source resolution.
	Width  int `json:"width"`
	Height int `json:"height"`

	// Quality is the JPEG quality (1–31, lower = better). Default: 5.
	Quality int `json:"quality"`

	// OutputDir is relative to the publisher HLS directory.
	// E.g. "thumbnails" → written to {hls_dir}/{stream_code}/thumbnails/thumb.jpg
	OutputDir string `json:"output_dir"`
}
