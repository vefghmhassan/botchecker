package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/settings"
)

// maxImportSize bounds the uploaded file. A real export is a few kilobytes.
const maxImportSize = 1 << 20

// exportSettings downloads every setting as one file. It is a POST, not a
// link, because the passphrase must not end up in a URL, a proxy log or the
// browser history.
func (h *Handler) exportSettings(c *fiber.Ctx) error {
	if h.d.Settings == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "settings are not available")
	}
	now := time.Now()
	e, err := h.d.Settings.Export(c.FormValue("passphrase"), now)
	if err != nil {
		return redirectSettings(c, "", err.Error())
	}
	body, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}

	name := fmt.Sprintf("botchecker-settings-%s.json", now.In(h.cfg.Location).Format("20060102-1504"))
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSONCharsetUTF8)
	c.Set(fiber.HeaderContentDisposition, `attachment; filename="`+name+`"`)
	// The file may carry sealed tokens; nothing in between should keep a copy.
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Send(append(body, '\n'))
}

// importSettings applies an uploaded export.
func (h *Handler) importSettings(c *fiber.Ctx) error {
	if h.d.Settings == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "settings are not available")
	}
	lang := h.lang()

	fh, err := c.FormFile("file")
	if err != nil {
		return redirectSettings(c, "", i18n.T(lang, "transfer.nofile"))
	}
	if fh.Size > maxImportSize {
		return redirectSettings(c, "", i18n.T(lang, "transfer.toobig"))
	}
	f, err := fh.Open()
	if err != nil {
		return redirectSettings(c, "", err.Error())
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxImportSize+1))
	if err != nil {
		return redirectSettings(c, "", err.Error())
	}

	e, err := settings.ParseExport(data)
	if err != nil {
		return redirectSettings(c, "", importError(lang, err))
	}
	res, err := h.d.Settings.Import(e, c.FormValue("passphrase"))
	if err != nil {
		return redirectSettings(c, "", importError(lang, err))
	}
	return redirectSettings(c, importSummary(lang, res), "")
}

// importError translates the failures a person can do something about.
func importError(lang i18n.Lang, err error) string {
	switch {
	case errors.Is(err, settings.ErrNotAnExport):
		return i18n.T(lang, "transfer.notexport")
	case errors.Is(err, settings.ErrNeedPassphrase):
		return i18n.T(lang, "transfer.needpass")
	case errors.Is(err, settings.ErrWrongPassphrase):
		return i18n.T(lang, "transfer.wrongpass")
	}
	return err.Error()
}

// importSummary says what changed, and what still has to be entered by hand.
func importSummary(lang i18n.Lang, res settings.ImportResult) string {
	parts := []string{i18n.T(lang, "transfer.done", len(res.Applied), res.Unchanged)}
	if len(res.LeftOut) > 0 {
		parts = append(parts, i18n.T(lang, "transfer.leftout", strings.Join(res.LeftOut, ", ")))
	}
	if len(res.Unknown) > 0 {
		parts = append(parts, i18n.T(lang, "transfer.unknown", strings.Join(res.Unknown, ", ")))
	}
	return strings.Join(parts, " ")
}
