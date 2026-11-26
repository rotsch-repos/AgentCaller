//go:build !unix

package main

// lockStore is not available here: nothing keeps two processes off one store.
func lockStore(string) (release func(), err error) {
	return func() {}, nil
}
