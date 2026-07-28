package model

import (
	"strconv"
	"strings"
	"testing"
)

// Une erreur d'outil qui nomme une propriete manquante n'est pas exploitable
// sans la charge qui l'a omise. On verifie que l'apercu survit du demarrage de
// l'appel jusqu'a son echec, qu'il est borne, redige, et consomme une seule
// fois.
func TestRememberAndTakeInput(t *testing.T) {
	h := &storeHooks{}

	h.rememberInput("tool-1", []byte(`{"a":1}`))
	if got := h.takeInput("tool-1"); got != `{"a":1}` {
		t.Errorf("apercu perdu entre le demarrage et l'echec: %q", got)
	}
	// Consomme : un second appel ne doit plus rien rendre, sinon un apercu
	// s'accrocherait a un appel ulterieur portant le meme identifiant.
	if got := h.takeInput("tool-1"); got != "" {
		t.Errorf("apercu non consomme: %q", got)
	}

	if h.takeInput("jamais-vu") != "" {
		t.Error("un identifiant inconnu doit rendre une chaine vide")
	}
	h.rememberInput("", []byte(`{"a":1}`))
	h.rememberInput("vide", nil)
	if h.takeInput("vide") != "" {
		t.Error("une entree vide ne doit rien memoriser")
	}
}

func TestInputPreviewIsBoundedAndRedacted(t *testing.T) {
	h := &storeHooks{red: func(s string) string { return strings.ReplaceAll(s, "hunter2", "<REDACTED>") }}

	h.rememberInput("long", []byte(strings.Repeat("x", toolInputPreviewMax*3)))
	got := h.takeInput("long")
	if len(got) > toolInputPreviewMax+len("…") {
		t.Errorf("apercu non borne: %d caracteres", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("une troncature doit se voir")
	}

	// Le guard s'applique a l'apercu : la ligne de log ne doit pas devenir un
	// canal de fuite parce qu'un appel a echoue.
	h.rememberInput("secret", []byte(`{"token":"hunter2"}`))
	if got := h.takeInput("secret"); strings.Contains(got, "hunter2") {
		t.Errorf("secret non redige dans l'apercu: %q", got)
	}
}

func TestRecentInputsStaysBounded(t *testing.T) {
	h := &storeHooks{}
	// Des appels qui ne se terminent jamais ne doivent pas faire enfler la map.
	for i := 0; i < maxRecentInputs*4; i++ {
		h.rememberInput(string(rune('a'+i%26))+strconv.Itoa(i), []byte(`{"a":1}`))
	}
	h.inputsMu.Lock()
	n := len(h.recentInputs)
	h.inputsMu.Unlock()
	if n > maxRecentInputs {
		t.Errorf("map non bornee: %d entrees pour un plafond de %d", n, maxRecentInputs)
	}
}
