package collection

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/go-logr/logr"
	yml "gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Collection archive manifest.yaml
type CollectionManifest struct {
	Contents []CollectionContents `yaml:"contents,omitempty"`
}

type CollectionContents struct {
	File   string `yaml:"file,omitempty"`
	Sha256 string `yaml:"sha256,omitempty"`
}

// This is the rendered asset, including its sha256 from the manifest.
type CollectionAsset struct {
	Name string
	Sha256 string
	Yaml unstructured.Unstructured
}

func DownloadToByte(url string) ([]byte, error) {
	r, err := http.Get(url)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("Could not download file: %v", url))
	}
	defer r.Body.Close()
	b, err := ioutil.ReadAll(r.Body)
	return b, err
}

//Read the manifests from a tar.gz archive
//It would be better to use the manifest.yaml as the index, and check the signatures
//For now, ignore manifest.yaml and return all other yaml files from the archive
func decodeManifests(archive []byte, renderingContext map[string]interface{}, reqLogger logr.Logger) ([]CollectionAsset, error) {
	manifests := []CollectionAsset{}
	var collectionmanifest CollectionManifest

	// Read the manifest.yaml from the collection archive
	r := bytes.NewReader(archive)
	gzReader, err := gzip.NewReader(r)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("Could not read manifest gzip"))
	}
	tarReader := tar.NewReader(gzReader)

	foundManifest := false
	var headers []string
	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, errors.New(fmt.Sprintf("Could not read manifest tar"))
		}

		headers = append(headers, header.Name)

		switch {
		case strings.TrimPrefix(header.Name, "./") == "manifest.yaml":
			//Buffer the document for further processing
			b := make([]byte, header.Size)
			i, err := tarReader.Read(b)
			//An EOF error is normal, as long as bytes read > 0
			if err == io.EOF && i == 0 || err != nil && err != io.EOF {
				return nil, fmt.Errorf("Error reading archive %v: %v", header.Name, err.Error())
			}
			err = yml.Unmarshal(b, &collectionmanifest)
			if err != nil {
				return nil, err
			}
			foundManifest = true
		}
	}

	fmt.Println("Header names: ", strings.Join(headers, ","))

	if foundManifest != true {
		return nil, fmt.Errorf("Error reading archive, unable to read manifest.yaml")
	}

	// Re-Read the archive and validate against archive manifest.yaml
	r = bytes.NewReader(archive)
	gzReader, err = gzip.NewReader(r)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("Could not read manifest gzip"))
	}
	tarReader = tar.NewReader(gzReader)

	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, errors.New(fmt.Sprintf("Could not read manifest tar"))
		}

		// Ignore manifest.yaml on this pass, only read yaml files
		switch {
		case strings.TrimPrefix(header.Name, "./") == "manifest.yaml":
			break
		case strings.HasSuffix(header.Name, ".yaml"):
			//Buffer the document for further processing
			b := make([]byte, header.Size)
			i, err := tarReader.Read(b)
			//An EOF error is normal, as long as bytes read > 0
			if err == io.EOF && i == 0 || err != nil && err != io.EOF {
				return nil, fmt.Errorf("Error reading archive %v: %v", header.Name, err.Error())
			}

			// Checksum. Lookup the read file in the index and compare sha256
			match := false
			b_sum := sha256.Sum256(b)
			assetSumString := ""
			for _, content := range collectionmanifest.Contents {
				if content.File == strings.TrimPrefix(header.Name, "./") {
					// Older releases may not have a sha256 in the manifest.yaml
					assetSumString = content.Sha256
					if content.Sha256 != "" {
						var c_sum [32]byte
						decoded, err := hex.DecodeString(content.Sha256)
						if err != nil {
							return nil, err
						}
						copy(c_sum[:], decoded)
						if b_sum != c_sum {
							return nil, fmt.Errorf("Archive file: %v  manifest.yaml checksum: %x  did not match file checksum: %x", header.Name, c_sum, b_sum)
						}
						match = true
					} else {
						// Would be nice if we could make this a warning message, but it seems like the only
						// options are error and info.  It's possible that some implementation has other methods
						// but someone needs to investigate.
						reqLogger.Info(fmt.Sprintf("Archive file %v was listed in the manifest but had no checksum.  Checksum validation for this file is skipped.", header.Name))
						match = true
					}
				}
			}
			if match != true {
				return nil, fmt.Errorf("File %v was found in the archive, but not in the manifest.yaml", header.Name)
			}

			//Apply the Kabanero yaml directive processor
			s := &DirectiveProcessor{}
			b, err = s.Render(b, renderingContext)
			if err != nil {
				return nil, fmt.Errorf("Error processing directives %v: %v", header.Name, err.Error())
			}

			decoder := yaml.NewYAMLToJSONDecoder(bytes.NewReader(b))
			out := unstructured.Unstructured{}
			err = decoder.Decode(&out)
			if err != nil {
				return nil, fmt.Errorf("Error decoding %v: %v", header.Name, err.Error())
			}
			manifests = append(manifests, CollectionAsset{Name: header.Name, Yaml: out, Sha256: assetSumString})
		}
	}
	return manifests, nil
}

func GetManifests(url string, checksum string, renderingContext map[string]interface{}, reqLogger logr.Logger) ([]CollectionAsset, error) {
	b, err := DownloadToByte(url)
	if err != nil {
		return nil, err
	}

	b_sum := sha256.Sum256(b)
	var c_sum [32]byte
	decoded, err := hex.DecodeString(checksum)
	if err != nil {
		return nil, err
	}
	copy(c_sum[:], decoded)

	if b_sum != c_sum {
		return nil, fmt.Errorf("Index checksum: %x not match download checksum: %x", c_sum, b_sum)
	}

	manifests, err := decodeManifests(b, renderingContext, reqLogger)
	if err != nil {
		return nil, err
	}
	return manifests, err
}
