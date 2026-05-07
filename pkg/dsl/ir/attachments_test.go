package ir

import "testing"

const attachmentsSrc = `
schema empty:
  ok: bool

prompt sys:
  System.

prompt usr:
  Look at {{attachments.logo}} and read {{attachments.spec.path}} (mime {{attachments.spec.mime}}).

attachments:
  logo: image
  spec: file
    description: "Spec PDF"
    accept_mime: ["application/pdf"]
    required: true

agent reviewer:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr

workflow demo:
  entry: reviewer
  reviewer -> done
`

func TestCompileAttachments_Roundtrip(t *testing.T) {
	w := mustCompile(t, attachmentsSrc)

	if len(w.Attachments) != 2 {
		t.Fatalf("expected 2 attachments, got %d", len(w.Attachments))
	}
	logo, ok := w.Attachments["logo"]
	if !ok {
		t.Fatal("logo missing")
	}
	if logo.Type != AttachmentImage {
		t.Errorf("logo.Type = %v want image", logo.Type)
	}
	if logo.Required {
		t.Errorf("logo.Required defaults to false")
	}
	spec := w.Attachments["spec"]
	if spec == nil {
		t.Fatal("spec missing")
	}
	if !spec.Required {
		t.Errorf("spec.Required = false, want true")
	}
	if spec.Description != "Spec PDF" {
		t.Errorf("spec.Description = %q", spec.Description)
	}
	if len(spec.AcceptMIME) != 1 || spec.AcceptMIME[0] != "application/pdf" {
		t.Errorf("spec.AcceptMIME = %v", spec.AcceptMIME)
	}
}

func TestValidateAttachment_Unknown(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  System.

prompt usr:
  Use {{attachments.missing}}.

agent a:
  model: "m"
  input: empty
  output: empty
  system: sys
  user: usr

workflow w:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagUnknownAttachment)
}

func TestValidateAttachment_BadSubField(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  System.

prompt usr:
  Use {{attachments.logo.bogus}}.

attachments:
  logo: image

agent a:
  model: "m"
  input: empty
  output: empty
  system: sys
  user: usr

workflow w:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagAttachmentSubfieldUnknown)
}

func TestValidateAttachment_VarConflict(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  System.

prompt usr:
  Hi.

vars:
  logo: string

attachments:
  logo: image

agent a:
  model: "m"
  input: empty
  output: empty
  system: sys
  user: usr

workflow w:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagAttachmentVarConflict)
}

func TestValidateAttachment_DuplicateAndBadMIME(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  System.

prompt usr:
  Hi.

attachments:
  logo: image
  logo: file
  bad: file
    accept_mime: ["application", "text/plain"]

agent a:
  model: "m"
  input: empty
  output: empty
  system: sys
  user: usr

workflow w:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagDuplicateAttachment)
	expectDiag(t, r, DiagInvalidAttachmentMIME)
}
