package auth

import (
	"context"
	"fmt"
	"html"
	"strings"
)

// mail is one transactional message: a short title, one line of context, a
// single link, and how long it lasts. Both the plain-text and the HTML body
// come from it, so the two never drift.
type mail struct {
	Subject string
	Title   string // the heading: "You're invited to ws"
	Intro   string // one sentence above the button
	Action  string // the button and link text
	Link    string
	Expires string // "7 days", "15 minutes"
}

// text is the plain body: everything the HTML says, link included, for
// clients that show text first.
func (m mail) text() string {
	return fmt.Sprintf("%s\n\n%s\n\n%s:\n%s\n\nThe link expires in %s. If you weren't expecting this, you can ignore it.\n", m.Title, m.Intro, m.Action, m.Link, m.Expires)
}

// html is the branded body, docs/DESIGN.md in email form: warm paper,
// dark-brown ink, Georgia for what is read, one accent for the link and
// the button, hairlines instead of shadows. Everything is inline and
// table-based because mail clients ignore stylesheets and most layout; the
// colors are the light-theme tokens from web/src/index.css, written out
// since email cannot read them. The link is repeated as text under the
// button for clients that strip buttons.
func (m mail) html() string {
	const (
		bg     = "#faf8f5"
		fg     = "#2c2416"
		fg2    = "#6b5f4f"
		fg3    = "#8a7e6e"
		line   = "#d8d0c4"
		accent = "#b85a1a"
		serif  = "Georgia, 'Bitstream Charter', 'Times New Roman', serif"
		sans   = "ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif"
	)
	e := html.EscapeString
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>%s</title></head>
<body style="margin:0;padding:0;background:%s;">
<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" border="0" style="background:%s;">
<tr><td align="center" style="padding:40px 16px;">
<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" border="0" style="max-width:520px;">
<tr><td style="padding:0 0 20px 0;font-family:%s;font-size:13px;letter-spacing:.3em;text-transform:uppercase;color:%s;">ws</td></tr>
<tr><td style="border-top:1px solid %s;"></td></tr>
<tr><td style="padding:28px 0 8px 0;font-family:%s;font-size:24px;line-height:1.3;color:%s;">%s</td></tr>
<tr><td style="padding:0 0 24px 0;font-family:%s;font-size:17px;line-height:1.65;color:%s;">%s</td></tr>
<tr><td style="padding:0 0 24px 0;">
<table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr>
<td style="background:%s;border-radius:6px;">
<a href="%s" style="display:inline-block;padding:11px 20px;font-family:%s;font-size:15px;font-weight:600;color:#ffffff;text-decoration:none;">%s</a>
</td></tr></table>
</td></tr>
<tr><td style="padding:0 0 28px 0;font-family:%s;font-size:14px;line-height:1.6;color:%s;">Or paste this address into your browser:<br><a href="%s" style="color:%s;text-decoration:underline;text-decoration-color:#c9a882;word-break:break-all;">%s</a></td></tr>
<tr><td style="border-top:1px solid %s;"></td></tr>
<tr><td style="padding:16px 0 0 0;font-family:%s;font-size:13px;line-height:1.6;color:%s;">The link expires in %s. If you weren't expecting this message, you can ignore it; nothing happens unless the link is opened.</td></tr>
</table>
</td></tr>
</table>
</body>
</html>
`,
		e(m.Subject), bg, bg,
		serif, fg3,
		line,
		serif, fg, e(m.Title),
		serif, fg, e(m.Intro),
		accent, e(m.Link), sans, e(m.Action),
		sans, fg2, e(m.Link), accent, e(m.Link),
		line,
		sans, fg3, e(m.Expires),
	)
	return b.String()
}

// send delivers m through the mailer, if there is one.
func (s *Service) send(ctx context.Context, to string, m mail) error {
	if s.mailer == nil {
		return nil
	}
	return s.mailer.Send(ctx, to, m.Subject, m.text(), m.html())
}
