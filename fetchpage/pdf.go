package fetchpage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	pdfMaxPages       = 40
	pdfMaxOutputBytes = 2 << 20
	// pdfTimeout bounds one conversion even when the caller set no
	// deadline; a malformed PDF can keep pdftotext busy indefinitely.
	pdfTimeout = 30 * time.Second
)

// PDFToText converts a PDF to plain text. The default runs Poppler's
// pdftotext on the first 40 pages; when the binary is not installed, PDFs
// fail with ErrPDFUnsupported. Replace it to use another converter, or set
// it to nil to refuse PDFs.
var PDFToText func(ctx context.Context, pdf []byte) (string, error) = popplerPDFToText

func popplerPDFToText(ctx context.Context, pdf []byte) (string, error) {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", ErrPDFUnsupported
	}

	// CreateTemp opens the file 0600, so other users cannot read the
	// document while it is being converted.
	f, err := os.CreateTemp("", "fetchpage-*.pdf")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())

	if _, err := f.Write(pdf); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	convertCtx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()

	out := &capped{limit: pdfMaxOutputBytes}
	cmd := exec.CommandContext(convertCtx, bin, "-q", "-enc", "UTF-8", "-nopgbrk", "-l", strconv.Itoa(pdfMaxPages), f.Name(), "-")
	cmd.Stdout = out
	cmd.WaitDelay = time.Second

	if err := cmd.Run(); err != nil {
		switch {
		case ctx.Err() != nil:
			return "", ctx.Err()
		case convertCtx.Err() != nil:
			// Not the caller's deadline: saying so keeps a slow PDF from
			// reading as the whole request having run out of time.
			return "", fmt.Errorf("pdftotext berhenti setelah %v", pdfTimeout)
		}
		return "", fmt.Errorf("pdftotext: %w", err)
	}

	return out.String(), nil
}

func documentFromPDF(ctx context.Context, body []byte, u *url.URL) (*document, error) {
	if PDFToText == nil {
		return nil, ErrPDFUnsupported
	}

	text, err := PDFToText(ctx, body)
	if err != nil {
		return nil, err
	}

	blocks := textBlocks(text)
	if len(blocks) == 0 {
		// A scanned PDF holds images of text and no text layer.
		return nil, fmt.Errorf("%w: PDF tidak punya lapisan teks (kemungkinan hasil scan)", ErrNoContent)
	}

	title := firstLineTitle(blocks)
	if title == "" {
		title = path.Base(u.Path)
	}

	return &document{title: title, source: SourcePDF, blocks: blocks}, nil
}

// capped is a bytes.Buffer that keeps the first limit bytes and quietly
// drops the rest, so a huge document cannot exhaust memory.
type capped struct {
	bytes.Buffer
	limit int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func isPDF(mediaType string, u *url.URL) bool {
	return mediaType == "application/pdf" ||
		(mediaType == "application/octet-stream" && strings.HasSuffix(strings.ToLower(u.Path), ".pdf"))
}

var errTooLarge = errors.New("konten terlalu besar")
