package sep2embed

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The three refusal conditions startup classification can produce. They
// are sentinels rather than bare fmt.Errorf strings because they are the
// contract this package fails closed on, and because a test that proves
// "the bridge refused" must prove WHICH refusal it was: "partially
// populated" and "empty and unwritable" call for different operator
// action, and matching on message text would let a reworded message pass
// a test that no longer checks anything.
var (
	// errCertDirPartial reports a directory holding some but not all of
	// the certificate files the configured mode requires. It is fatal
	// even when the directory is writable: see classifyCertDir.
	errCertDirPartial = errors.New("sep2embed: certificate directory is partially populated")

	// errCertDirNotWritable reports a directory holding none of the
	// required certificate files that this process also cannot write to,
	// so it can neither load nor mint an identity.
	errCertDirNotWritable = errors.New("sep2embed: certificate directory holds no certificate material and cannot be written to")

	// errCertDirUnreadable reports that the directory's contents could
	// not be determined at all: a permission or I/O error, NOT a missing
	// file. Keeping this distinct from absence is the point; see
	// certFileExists.
	errCertDirUnreadable = errors.New("sep2embed: certificate directory could not be inspected")
)

// allServerCertFileNames is every file name this package manages inside a
// cert dir, in a stable order.
//
// Emptiness is judged against this full set rather than against the
// mode's required subset. A directory holding only ca-key.pem is not
// empty just because the configured mode does not require that file: an
// operator put something there, and minting a fresh set alongside it
// would produce a directory whose CA certificate and CA key do not match.
var allServerCertFileNames = []string{caCertFileName, caKeyFileName, serverCertFileName, serverKeyFileName}

// requiresCASigningKey reports whether mode signs certificates in this
// process and therefore needs the CA private key present on the host.
//
// DeviceCertModeDevMint signs: it mints the server leaf against the CA,
// and mints device certificates against the same CA (see
// loadDeviceSigningCA). DeviceCertModePreprovisioned never signs
// anything; it reads only the CA's public certificate, to verify chains.
// Requiring the CA signing key in that mode purely to satisfy a presence
// check would defeat the least-privilege split the mode exists for, and
// treating its absence as "incomplete" is exactly what caused
// ensureServerIdentity to mint development material over an operator's
// real server certificate and private key.
//
// Any value that is not DeviceCertModePreprovisioned is treated as
// signing, so an out-of-range DeviceCertMode fails toward the stricter
// required set (a refusal to start) rather than the looser one.
func requiresCASigningKey(mode DeviceCertMode) bool {
	return mode != DeviceCertModePreprovisioned
}

// requiredServerCertFiles returns the file names that must already exist
// under a cert dir for mode to start without minting, in a stable order.
func requiredServerCertFiles(mode DeviceCertMode) []string {
	if requiresCASigningKey(mode) {
		return []string{caCertFileName, caKeyFileName, serverCertFileName, serverKeyFileName}
	}
	return []string{caCertFileName, serverCertFileName, serverKeyFileName}
}

// certDirState is a cert directory's classification for one mode.
type certDirState int

const (
	// certDirComplete: every file the mode requires is present. Start,
	// write nothing. Whether the directory is writable is not consulted,
	// which is what makes a read-only bind mount a supported deployment.
	certDirComplete certDirState = iota

	// certDirEmpty: none of the managed file names is present. Eligible
	// for minting, if the directory can actually be written to.
	certDirEmpty

	// certDirPartial: some managed files are present but the mode's
	// required set is not satisfied. Never minted over.
	certDirPartial
)

// classifyCertDir inspects dir and reports whether it is complete for
// mode, empty, or partially populated, along with the managed files
// found and the required ones missing.
//
// It re-reads the filesystem on EVERY call and caches nothing, by
// design. Nothing in this package may memoize the result: a
// classification taken once at startup and consulted later would report
// a directory state that no longer exists. (The values it returns are
// still a point-in-time snapshot; callers act on them immediately, and
// the mint path additionally refuses to clobber at the syscall, so a
// concurrent writer cannot turn a stale "empty" into a destroyed file.)
//
// The partial state is fatal to the caller even when the directory is
// writable, and that is the deliberate choice: "some files present" most
// often means an operator supplied real material and this process
// disagrees about which files it needs. Completing the set would mint a
// CA private key that does not match the CA certificate already sitting
// there, producing a directory that looks provisioned and cannot
// verify a single chain. Refusing costs a startup failure with a message
// naming the exact files; guessing costs a trust anchor.
func classifyCertDir(dir string, mode DeviceCertMode) (state certDirState, present, missing []string, err error) {
	found := make(map[string]bool, len(allServerCertFileNames))
	for _, name := range allServerCertFileNames {
		exists, existsErr := certFileExists(filepath.Join(dir, name))
		if existsErr != nil {
			return 0, nil, nil, existsErr
		}
		found[name] = exists
		if exists {
			present = append(present, name)
		}
	}

	for _, name := range requiredServerCertFiles(mode) {
		if !found[name] {
			missing = append(missing, name)
		}
	}

	switch {
	case len(missing) == 0:
		return certDirComplete, present, nil, nil
	case len(present) == 0:
		return certDirEmpty, nil, missing, nil
	default:
		return certDirPartial, present, missing, nil
	}
}

// certFileExists reports whether path names an existing regular file.
//
// Only fs.ErrNotExist counts as absence. Every other Stat failure,
// notably EACCES on the file or on a parent directory, is returned as an
// error. A permission problem is NOT a missing file: conflating the two
// is what let an unreadable but fully provisioned directory present
// itself as empty and be minted into.
func certFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if info.IsDir() {
			return false, fmt.Errorf("%w: %q is a directory, not a certificate file", errCertDirUnreadable, path)
		}
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("%w: cannot determine whether %q exists; this is a permission or I/O error, not a missing file, so it is NOT treated as absent and nothing will be written over it: %w",
			errCertDirUnreadable, path, err)
	}
}

// certDirWriteProbeName is the pattern for the throwaway file
// certDirWritable creates. The leading dot keeps it out of the way of
// any directory listing an operator eyeballs, and the fixed prefix makes
// a leftover attributable to this process if a cleanup ever fails.
const certDirWriteProbeName = ".sep2embed-write-probe-*"

// certDirWritable reports whether this process can actually create a
// file in dir, creating dir first if it does not exist.
//
// Writability is determined by ATTEMPTING a write, never by reading mode
// bits. Mode bits are not the authority on this question: ownership,
// POSIX ACLs, a read-only mount, SELinux, and container uid mapping can
// each make a directory that looks 0700 to this process unwritable by
// it, or the reverse. The only honest test is the syscall.
//
// The probe file is removed before this returns, on every path that
// created one. A probe that could not be removed is reported as an
// error rather than ignored: leaving stray files in an operator's
// certificate directory is not an acceptable side effect of a check.
func certDirWritable(dir string) error {
	if err := os.MkdirAll(dir, certDirPerm); err != nil {
		return fmt.Errorf("create certificate directory %q: %w", dir, err)
	}

	probe, err := os.CreateTemp(dir, certDirWriteProbeName)
	if err != nil {
		return fmt.Errorf("write probe in certificate directory %q: %w", dir, err)
	}
	probeName := probe.Name()

	closeErr := probe.Close()
	if rmErr := os.Remove(probeName); rmErr != nil {
		return fmt.Errorf("remove write probe %q: %w", probeName, rmErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close write probe %q: %w", probeName, closeErr)
	}
	return nil
}

// writeFileNoClobber writes data to path atomically, and ONLY if path
// does not already exist.
//
// It is writeFileAtomic's fail-if-present twin, and it is what the
// server-identity mint path uses instead. writeFileAtomic finishes with
// os.Rename, which silently REPLACES an existing file: that syscall is
// the mechanism by which operator-supplied certificate and private key
// material was destroyed. Classifying the directory first closes the
// ordinary case, but a classification is a check and a write is an act,
// and between them there is a window. Finishing with os.Link instead
// closes that window in the kernel: link fails with EEXIST rather than
// replacing, so there is no interleaving in which this function can
// destroy a file.
//
// The temp file is created in path's own directory, so the link never
// crosses a filesystem. A filesystem that cannot hard link fails here
// rather than falling back to a replacing write: minting is a
// development convenience, and no convenience justifies reopening the
// overwrite path.
func writeFileNoClobber(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// The temp file is always removed: unlike writeFileAtomic's
		// rename, os.Link leaves the source in place, so this runs on the
		// success path too.
		if rmErr := os.Remove(tmpName); rmErr != nil && err == nil {
			err = fmt.Errorf("remove temp file %q: %w", tmpName, rmErr)
		}
	}()

	if _, writeErr := tmp.Write(data); writeErr != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", writeErr)
	}
	if closeErr := tmp.Close(); closeErr != nil {
		return fmt.Errorf("close temp file: %w", closeErr)
	}
	if chmodErr := os.Chmod(tmpName, perm); chmodErr != nil {
		return fmt.Errorf("chmod temp file: %w", chmodErr)
	}
	if linkErr := os.Link(tmpName, path); linkErr != nil {
		if errors.Is(linkErr, fs.ErrExist) {
			return fmt.Errorf("refusing to write %q: a file already exists there and this process never overwrites certificate material: %w", path, linkErr)
		}
		return fmt.Errorf("link temp file to %s: %w", path, linkErr)
	}

	return nil
}

// describeCertFiles renders a file-name list for an operator-facing
// error message.
func describeCertFiles(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}
