package mail

import (
	"fmt"
	"html/template"
	"strings"
)

// ResetData parameterises the password-reset email.
type ResetData struct {
	// ResetURL is the SPA link carrying the one-shot token, e.g.
	// https://iterion.example.org/auth/reset?token=iar_…
	ResetURL string
	// ExpiresMinutes is the token lifetime for the copy.
	ExpiresMinutes int
}

// InviteData parameterises the invitation email.
type InviteData struct {
	TeamName string
	Role     string
	// OrgName and OrgRole are the team's parent org and the org membership
	// accepting writes there. The message names them only for a role above
	// member — the one an invitee must know they are being handed.
	OrgName string
	OrgRole string
	// AcceptURL is the SPA link carrying the invitation token.
	AcceptURL string
	// InvitedBy is a display hint (email of the inviting admin).
	InvitedBy string
}

// RenderPasswordReset builds the password-reset message for `to`.
func RenderPasswordReset(to string, d ResetData) Message {
	text := fmt.Sprintf(`Someone (hopefully you) requested a password reset for this iterion account.

Reset your password: %s

The link expires in %d minutes and can be used once. If you didn't
request this, ignore this email — your password is unchanged.`, d.ResetURL, d.ExpiresMinutes)
	html := fmt.Sprintf(`<p>Someone (hopefully you) requested a password reset for this iterion account.</p>
<p><a href="%s">Reset your password</a></p>
<p>The link expires in %d minutes and can be used once. If you didn't request this, ignore this email — your password is unchanged.</p>`,
		template.HTMLEscapeString(d.ResetURL), d.ExpiresMinutes)
	return Message{To: to, Subject: "Reset your iterion password", TextBody: text, HTMLBody: html}
}

// RenderInvitation builds the team-invitation message for `to`.
func RenderInvitation(to string, d InviteData) Message {
	by := ""
	if d.InvitedBy != "" {
		by = fmt.Sprintf(" by %s", d.InvitedBy)
	}
	orgText, orgHTML := "", ""
	if d.OrgRole == "admin" || d.OrgRole == "owner" {
		org := strings.TrimSpace(d.OrgName)
		if org == "" {
			org = "its organization"
		} else {
			org = fmt.Sprintf("its organization %q", org)
		}
		orgText = fmt.Sprintf("\n\nAccepting also makes you %s of %s: every team in it, its members and its sign-in.", d.OrgRole, org)
		orgHTML = fmt.Sprintf("<p>Accepting also makes you <b>%s</b> of %s: every team in it, its members and its sign-in.</p>\n",
			template.HTMLEscapeString(d.OrgRole), template.HTMLEscapeString(org))
	}
	text := fmt.Sprintf(`You've been invited%s to join the iterion team %q as %s.%s

Accept the invitation: %s

If you don't have an account yet, the link lets you register with
this email address.`, by, d.TeamName, d.Role, orgText, d.AcceptURL)
	html := fmt.Sprintf(`<p>You've been invited%s to join the iterion team <b>%s</b> as <b>%s</b>.</p>
%s<p><a href="%s">Accept the invitation</a></p>
<p>If you don't have an account yet, the link lets you register with this email address.</p>`,
		template.HTMLEscapeString(by), template.HTMLEscapeString(d.TeamName),
		template.HTMLEscapeString(d.Role), orgHTML, template.HTMLEscapeString(d.AcceptURL))
	return Message{To: to, Subject: fmt.Sprintf("Invitation to join %s on iterion", strings.TrimSpace(d.TeamName)), TextBody: text, HTMLBody: html}
}
