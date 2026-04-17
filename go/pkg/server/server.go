package server

import extension "activity-reward-extension/internal/extension"

// StartExtension creates and starts the template extension server in a goroutine.
// Returns an error channel that receives any ListenAndServe failure (e.g., port already in use).
func StartExtension(extensionPort, signPort int) <-chan error {
	e := extension.New(extensionPort, signPort)
	errCh := make(chan error, 1)
	go func() {
		if err := e.Server.ListenAndServe(); err != nil {
			errCh <- err
		}
	}()
	return errCh
}
