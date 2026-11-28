package httpapi

import (
	"fmt"
	"io"
	"net/http"

	"yanshi/internal/service"
	"yanshi/internal/speech"
)

func (s *Server) transcribe(w http.ResponseWriter, r *http.Request) {
	st, ok := s.session(w, r)
	if !ok {
		return
	}
	audio, err := io.ReadAll(http.MaxBytesReader(w, r.Body, speech.MaxAudioBytes))
	if err != nil {
		s.fail(w, fmt.Errorf("%w: audio larger than %d bytes", service.ErrInvalid, speech.MaxAudioBytes))
		return
	}
	text, err := s.Speech.Transcribe(r.Context(), st, audio)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

func (s *Server) speak(w http.ResponseWriter, r *http.Request) {
	st, ok := s.session(w, r)
	if !ok {
		return
	}
	wav, err := s.Speech.Speak(r.Context(), st, r.PathValue("run"))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(wav)
}
