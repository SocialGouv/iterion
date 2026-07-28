package server

import "fmt"

// resolveResumeSource turns a run's persisted file path (plus any inline
// source the caller already has) into the (absolute path, source) pair
// runview.ResumeSpec needs.
//
// Shared by the operator-initiated resume handler and the retry sweeper so
// the cloud-mode rule lives in one place: a server pod has no operator
// filesystem, so a resume must carry inline source UNLESS the path names a
// catalog bundle baked into the image, which the pod can read itself.
// Duplicating that rule is how the automated path would quietly diverge
// from the manual one.
func (s *Server) resolveResumeSource(filePath, source string) (string, string, error) {
	if filePath == "" && source == "" {
		return "", "", fmt.Errorf("file_path or source is required (run has no persisted FilePath)")
	}
	if s.cfg.Mode == "cloud" && source == "" {
		if src, _, _, ok := s.catalogBotSource("", filePath); ok {
			source = src
		} else {
			return "", "", fmt.Errorf("cloud mode: source or a catalog bot is required (file_path is not portable across the server pod's filesystem)")
		}
	}
	absPath, err := s.resolveWorkflowPath(filePath, source)
	if err != nil {
		return "", "", fmt.Errorf("invalid file_path: %w", err)
	}
	return absPath, source, nil
}
