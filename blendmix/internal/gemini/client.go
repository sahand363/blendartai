package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"strings"

	_ "golang.org/x/image/webp"
	"google.golang.org/genai"
)

const maxSourceSide = 1024

type PreparedImage struct {
	Data     []byte
	MIMEType string
}

type DescribeResult struct {
	Prompts []string `json:"prompts"`
	RawJSON []byte   `json:"-"`
}

type Generator interface {
	PrepareImage(path string) (PreparedImage, error)
	DescribePair(ctx context.Context, imgA, imgB PreparedImage, count int) (DescribeResult, error)
	GenerateImage(ctx context.Context, prompt string, imgA, imgB PreparedImage) ([]byte, string, error)
}

type Client struct {
	client     *genai.Client
	textModel  string
	imageModel string
	imageSize  string
}

func NewClient(ctx context.Context, apiKey, textModel, imageModel, imageSize string) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("empty gemini api key")
	}
	c, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("create genai client: %w", err)
	}
	return &Client{
		client:     c,
		textModel:  textModel,
		imageModel: imageModel,
		imageSize:  imageSize,
	}, nil
}

// PrepareImage decodes a PNG, JPEG, or WebP file, downscales it so the longest
// side is <= 1024px, and encodes it as PNG or JPEG for sending to Gemini.
func (c *Client) PrepareImage(path string) (PreparedImage, error) {
	f, err := os.Open(path)
	if err != nil {
		return PreparedImage{}, err
	}
	defer f.Close()

	src, format, err := image.Decode(f)
	if err != nil {
		return PreparedImage{}, fmt.Errorf("decode %s: %w", path, err)
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > maxSourceSide || h > maxSourceSide {
		var nw, nh int
		if w >= h {
			nw = maxSourceSide
			nh = max(1, (h*maxSourceSide)/w)
		} else {
			nh = maxSourceSide
			nw = max(1, (w*maxSourceSide)/h)
		}
		src = resizeBilinear(src, nw, nh)
	}

	var buf bytes.Buffer
	if format == "png" {
		if err := png.Encode(&buf, src); err != nil {
			return PreparedImage{}, err
		}
		return PreparedImage{Data: buf.Bytes(), MIMEType: "image/png"}, nil
	}

	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 90}); err != nil {
		return PreparedImage{}, err
	}
	return PreparedImage{Data: buf.Bytes(), MIMEType: "image/jpeg"}, nil
}

func (c *Client) DescribePair(ctx context.Context, imgA, imgB PreparedImage, count int) (DescribeResult, error) {
	instruction := fmt.Sprintf(
		"Analyze both provided reference images. Write %d DIFFERENT single-paragraph image generation prompts "+
			"that creatively blend the subjects, composition, mood, and visual style of both images into a cohesive new artwork. "+
			"Each prompt must be self-contained and MUST NEVER say \"first image\", \"second image\", \"image 1\", or \"image 2\". "+
			"Return strict JSON with the shape {\"prompts\": [\"...\"]}.",
		count,
	)

	contents := []*genai.Content{
		{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				genai.NewPartFromText(instruction),
				genai.NewPartFromBytes(imgA.Data, imgA.MIMEType),
				genai.NewPartFromBytes(imgB.Data, imgB.MIMEType),
			},
		},
	}

	cfg := &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
		ResponseSchema: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"prompts": {
					Type: genai.TypeArray,
					Items: &genai.Schema{
						Type: genai.TypeString,
					},
				},
			},
			Required: []string{"prompts"},
		},
	}

	resp, err := c.client.Models.GenerateContent(ctx, c.textModel, contents, cfg)
	if err != nil {
		return DescribeResult{}, err
	}

	raw := strings.TrimSpace(resp.Text())
	if raw == "" {
		return DescribeResult{}, errors.New("empty describe response from text model")
	}

	var parsed DescribeResult
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return DescribeResult{}, fmt.Errorf("parse describe json: %w", err)
	}
	if len(parsed.Prompts) == 0 {
		return DescribeResult{}, errors.New("describe response contained no prompts")
	}
	parsed.RawJSON = []byte(raw)
	return parsed, nil
}

func (c *Client) GenerateImage(ctx context.Context, prompt string, imgA, imgB PreparedImage) ([]byte, string, error) {
	contents := []*genai.Content{
		{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				genai.NewPartFromText(prompt),
				genai.NewPartFromBytes(imgA.Data, imgA.MIMEType),
				genai.NewPartFromBytes(imgB.Data, imgB.MIMEType),
			},
		},
	}

	cfg := &genai.GenerateContentConfig{
		ResponseModalities: []string{"IMAGE"},
		ImageConfig: &genai.ImageConfig{
			ImageSize: c.imageSize,
		},
	}

	resp, err := c.client.Models.GenerateContent(ctx, c.imageModel, contents, cfg)
	if err != nil {
		return nil, "", err
	}

	for _, cand := range resp.Candidates {
		if cand == nil || cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			if part != nil && part.InlineData != nil && len(part.InlineData.Data) > 0 {
				return part.InlineData.Data, part.InlineData.MIMEType, nil
			}
		}
	}

	return nil, "", errors.New("image model returned no image data")
}

// IsQuotaError returns true for HTTP 429 or RESOURCE_EXHAUSTED errors.
func IsQuotaError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *genai.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Code == 429 || strings.EqualFold(apiErr.Status, "RESOURCE_EXHAUSTED") {
			return true
		}
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "RESOURCE_EXHAUSTED") || strings.Contains(msg, " 429")
}

// IsUnavailableError returns true for HTTP 503 or UNAVAILABLE errors.
func IsUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *genai.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Code == 503 || strings.EqualFold(apiErr.Status, "UNAVAILABLE") {
			return true
		}
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "UNAVAILABLE") || strings.Contains(msg, " 503")
}

func resizeBilinear(src image.Image, dstW, dstH int) *image.RGBA {
	sb := src.Bounds()
	srcW, srcH := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))

	xRatio := float64(srcW-1) / float64(max(1, dstW-1))
	yRatio := float64(srcH-1) / float64(max(1, dstH-1))

	for y := 0; y < dstH; y++ {
		sy := float64(y) * yRatio
		y0 := int(sy)
		y1 := min(y0+1, srcH-1)
		wy := sy - float64(y0)

		for x := 0; x < dstW; x++ {
			sx := float64(x) * xRatio
			x0 := int(sx)
			x1 := min(x0+1, srcW-1)
			wx := sx - float64(x0)

			c00 := color.RGBAModel.Convert(src.At(sb.Min.X+x0, sb.Min.Y+y0)).(color.RGBA)
			c10 := color.RGBAModel.Convert(src.At(sb.Min.X+x1, sb.Min.Y+y0)).(color.RGBA)
			c01 := color.RGBAModel.Convert(src.At(sb.Min.X+x0, sb.Min.Y+y1)).(color.RGBA)
			c11 := color.RGBAModel.Convert(src.At(sb.Min.X+x1, sb.Min.Y+y1)).(color.RGBA)

			dst.SetRGBA(x, y, color.RGBA{
				R: bilerp(c00.R, c10.R, c01.R, c11.R, wx, wy),
				G: bilerp(c00.G, c10.G, c01.G, c11.G, wx, wy),
				B: bilerp(c00.B, c10.B, c01.B, c11.B, wx, wy),
				A: bilerp(c00.A, c10.A, c01.A, c11.A, wx, wy),
			})
		}
	}
	return dst
}

func bilerp(c00, c10, c01, c11 uint8, wx, wy float64) uint8 {
	top := float64(c00)*(1-wx) + float64(c10)*wx
	bot := float64(c01)*(1-wx) + float64(c11)*wx
	v := top*(1-wy) + bot*wy
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v + 0.5)
}
