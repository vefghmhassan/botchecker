package dashboard_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func csrfToken(t *testing.T, app *fiber.App) string {
	t.Helper()
	_, body := get(t, app, "/settings", true)
	i := strings.Index(body, `name="csrf" value="`)
	if i < 0 {
		t.Fatal("no csrf token on the settings page")
	}
	rest := body[i+len(`name="csrf" value="`):]
	return rest[:strings.Index(rest, `"`)]
}

func postForm(t *testing.T, app *fiber.App, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	form.Set("csrf", csrfToken(t, app))
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", fiber.MIMEApplicationForm)
	req.SetBasicAuth(testUser, testPass)
	resp, err := app.Test(req, 10_000)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// Download from one dashboard, upload to another, through the real forms.
func TestSettingsMoveBetweenDashboards(t *testing.T) {
	from := newApp(t)
	postForm(t, from, "/settings", url.Values{
		"group":                {"telegram"},
		"telegram.chat_id":     {"100000001"},
		"telegram.bot_token":   {"123:secret-token"},
		"telegram.allowed_ids": {"100000001"},
	})

	resp, file := postForm(t, from, "/settings/export", url.Values{"passphrase": {"correct horse"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export = %d: %s", resp.StatusCode, file)
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Error("the export is not offered as a download")
	}
	if strings.Contains(file, "secret-token") {
		t.Fatal("the token is readable in the downloaded file")
	}

	to := newApp(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("csrf", csrfToken(t, to))
	_ = mw.WriteField("passphrase", "correct horse")
	fw, _ := mw.CreateFormFile("file", "settings.json")
	_, _ = fw.Write([]byte(file))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/settings/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetBasicAuth(testUser, testPass)
	res, err := to.Test(req, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSeeOther || strings.Contains(res.Header.Get("Location"), "err=") {
		t.Fatalf("import = %d → %s", res.StatusCode, res.Header.Get("Location"))
	}

	_, page := get(t, to, "/settings", true)
	if !strings.Contains(page, `value="100000001"`) {
		t.Error("the chat id did not arrive")
	}
	if !strings.Contains(page, "…oken") {
		t.Error("the bot token did not arrive")
	}
}
