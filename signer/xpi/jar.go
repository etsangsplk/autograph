package xpi

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"io"
	"io/ioutil"
	"path/filepath"
	"sort"
	"strings"
)

// getPriorityForFilename returns the priority for a filename
//
//     The filenames in a manifest are ordered so that files not in a
//     directory come before files in any directory, ordered
//     alphabetically but ignoring case, with a few exceptions
//     (install.rdf, chrome.manifest, icon.png and icon64.png come at the
//     beginning; licenses come at the end).
//
//     This order does not appear to affect anything in any way, but it
//     looks nicer.
func getPriorityForFilename(filename string) int {
	switch filename {
	case "install.rdf":
		return 1
	case "chrome.manifest", "icon.png", "icon64.png":
		return 2
	case "MPL", "GPL", "LGPL", "COPYING", "LICENSE", "license.txt":
		return 5
	default:
		return 4
	}
}

type byPriorityThenAlpha []*zip.File
func (s byPriorityThenAlpha) Len() int {
	return len(s)
}
func (s byPriorityThenAlpha) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s byPriorityThenAlpha) Less(i, j int) bool {
	iPriority, jPriority := getPriorityForFilename(s[i].Name), getPriorityForFilename(s[j].Name)
	if iPriority != jPriority {
		return iPriority < jPriority
	}

	iPath, iFile := filepath.Split(strings.ToLower(s[i].Name))
	jPath, jFile := filepath.Split(strings.ToLower(s[j].Name))

	if iPath != jPath {
		return iPath < jPath
	}
	return iFile < jFile
}

func makeJARManifests(input []byte) (manifest, sigfile []byte, err error) {
	inputReader := bytes.NewReader(input)
	r, err := zip.NewReader(inputReader, int64(len(input)))
	if err != nil {
		return
	}

	// first generate the manifest file by calculating md5, sha1, and sha256 hashes for each zip entry
	mw := bytes.NewBuffer(manifest)
	manifest = []byte(fmt.Sprintf("Manifest-Version: 1.0\n\n"))

	sort.Sort(byPriorityThenAlpha(r.File))
	for _, f := range r.File {
		if isSignatureFile(f.Name) {
			// reserved signature files do not get included in the manifest
			continue
		}
		if f.FileInfo().IsDir() {
			// directories do not get included
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return manifest, sigfile, err
		}
		data, err := ioutil.ReadAll(rc)
		if err != nil {
			return manifest, sigfile, err
		}
		fmt.Fprintf(mw, "Name: %s\nDigest-Algorithms: MD5 SHA1 SHA256\n", f.Name)
		h0 := md5.New()
		h0.Write(data)
		fmt.Fprintf(mw, "MD5-Digest: %s\n", base64.StdEncoding.EncodeToString(h0.Sum(nil)))
		h1 := sha1.New()
		h1.Write(data)
		fmt.Fprintf(mw, "SHA1-Digest: %s\n", base64.StdEncoding.EncodeToString(h1.Sum(nil)))
		h2 := sha256.New()
		h2.Write(data)
		fmt.Fprintf(mw, "SHA256-Digest: %s\n\n", base64.StdEncoding.EncodeToString(h2.Sum(nil)))
	}
	manifestBody := mw.Bytes()
	manifest = append(manifest, manifestBody...)

	// then calculate a signature file by hashing the manifest with sha1 and sha256
	sw := bytes.NewBuffer(sigfile)
	fmt.Fprint(sw, "Signature-Version: 1.0\n")
	h0 := md5.New()
	h0.Write(manifest)
	fmt.Fprintf(sw, "MD5-Digest-Manifest: %s\n", base64.StdEncoding.EncodeToString(h0.Sum(nil)))
	h1 := sha1.New()
	h1.Write(manifest)
	fmt.Fprintf(sw, "SHA1-Digest-Manifest: %s\n", base64.StdEncoding.EncodeToString(h1.Sum(nil)))
	h2 := sha256.New()
	h2.Write(manifest)
	fmt.Fprintf(sw, "SHA256-Digest-Manifest: %s\n\n", base64.StdEncoding.EncodeToString(h2.Sum(nil)))
	sigfile = sw.Bytes()

	return
}

// repackJAR inserts the manifest, signature file and pkcs7 signature in the input JAR file,
// and return a JAR ZIP archive
func repackJAR(input, manifest, sigfile, signature []byte) (output []byte, err error) {
	var (
		rc     io.ReadCloser
		fwhead *zip.FileHeader
		fw     io.Writer
		data   []byte
	)
	inputReader := bytes.NewReader(input)
	r, err := zip.NewReader(inputReader, int64(len(input)))
	if err != nil {
		return
	}
	// Create a buffer to write our archive to.
	buf := new(bytes.Buffer)

	// Create a new zip archive.
	w := zip.NewWriter(buf)

	// The PKCS7 file("foo.rsa") *MUST* be the first file in the
	// archive to take advantage of Firefox's optimized downloading
	// of XPIs
	fwhead = &zip.FileHeader{
		Name:   "META-INF/mozilla.rsa",
		Method: zip.Deflate,
	}
	fw, err = w.CreateHeader(fwhead)
	if err != nil {
		return
	}
	_, err = fw.Write(signature)
	if err != nil {
		return
	}

	// Iterate through the files in the archive,
	sort.Sort(byPriorityThenAlpha(r.File))
	for _, f := range r.File {
		// skip signature files, we have new ones we'll add at the end
		if isSignatureFile(f.Name) {
			continue
		}
		rc, err = f.Open()
		if err != nil {
			return
		}
		fwhead := &zip.FileHeader{
			Name:   f.Name,
			Method: zip.Deflate,
		}
		// insert the file into the archive
		fw, err = w.CreateHeader(fwhead)
		if err != nil {
			return
		}
		data, err = ioutil.ReadAll(rc)
		if err != nil {
			return
		}
		_, err = fw.Write(data)
		if err != nil {
			return
		}
		rc.Close()
	}

	// insert the remaining signature files. Those will be compressed
	// so we don't have to worry about their alignment
	var metas = []struct {
		Name string
		Body []byte
	}{
		{"META-INF/manifest.mf", manifest},
		{"META-INF/mozilla.sf", sigfile},
	}
	for _, meta := range metas {
		fwhead = &zip.FileHeader{
			Name:   meta.Name,
			Method: zip.Deflate,
		}
		fw, err = w.CreateHeader(fwhead)
		if err != nil {
			return
		}
		_, err = fw.Write(meta.Body)
		if err != nil {
			return
		}
	}
	// Make sure to check the error on Close.
	err = w.Close()
	if err != nil {
		return
	}

	output = buf.Bytes()
	return
}

// The JAR format defines a number of signature files stored under the META-INF directory
// META-INF/MANIFEST.MF
// META-INF/*.SF
// META-INF/*.DSA
// META-INF/*.RSA
// META-INF/SIG-*
// and their lowercase variants
// https://docs.oracle.com/javase/8/docs/technotes/guides/jar/jar.html#Signed_JAR_File
func isSignatureFile(name string) bool {
	if strings.HasPrefix(name, "META-INF/") {
		name = strings.TrimPrefix(name, "META-INF/")
		if name == "MANIFEST.MF" || name == "manifest.mf" ||
			strings.HasSuffix(name, ".SF") || strings.HasSuffix(name, ".sf") ||
			strings.HasSuffix(name, ".RSA") || strings.HasSuffix(name, ".rsa") ||
			strings.HasSuffix(name, ".DSA") || strings.HasSuffix(name, ".dsa") ||
			strings.HasPrefix(name, "SIG-") || strings.HasPrefix(name, "sig-") {
			return true
		}
	}
	return false
}

// mustReadFileFromZIP reads a given filename out of a ZIP and returns it or dies
func mustReadFileFromZIP(signedXPI []byte, filename string) (data []byte) {
	zipReader := bytes.NewReader(signedXPI)
	r, err := zip.NewReader(zipReader, int64(len(signedXPI)))
	if err != nil {
		log.Fatalf("Error reading ZIP %s", err)
	}

	for _, f := range r.File {
		if f.Name == filename {
			rc, err := f.Open()
			defer rc.Close()
			if err != nil {
				log.Fatalf("Error opening file %s in ZIP %s", filename, err)
			}
			data, err = ioutil.ReadAll(rc)
			if err != nil {
				log.Fatalf("Error reading file %s in ZIP %s", filename, err)
			}
			return
		}
	}
	log.Fatalf("failed to find %s in ZIP", filename)
	return
}
