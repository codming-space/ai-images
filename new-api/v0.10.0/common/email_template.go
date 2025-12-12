package common

import (
	"bytes"
	"embed"
	"html/template"
)

//go:embed templates/*.html
var emailTemplates embed.FS

var (
	emailVerificationTemplate *template.Template
	passwordResetTemplate     *template.Template
)

func init() {
	var err error
	emailVerificationTemplate, err = template.ParseFS(emailTemplates, "templates/email_verification.html")
	if err != nil {
		panic("failed to parse email verification template: " + err.Error())
	}

	passwordResetTemplate, err = template.ParseFS(emailTemplates, "templates/password_reset.html")
	if err != nil {
		panic("failed to parse password reset template: " + err.Error())
	}
}

type EmailVerificationData struct {
	Code                     string
	VerificationValidMinutes int
}

type PasswordResetData struct {
	Link                     string
	VerificationValidMinutes int
}

func RenderEmailVerificationTemplate(code string, validMinutes int) (string, error) {
	data := EmailVerificationData{
		Code:                     code,
		VerificationValidMinutes: validMinutes,
	}

	var buf bytes.Buffer
	if err := emailVerificationTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func RenderPasswordResetTemplate(link string, validMinutes int) (string, error) {
	data := PasswordResetData{
		Link:                     link,
		VerificationValidMinutes: validMinutes,
	}

	var buf bytes.Buffer
	if err := passwordResetTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
