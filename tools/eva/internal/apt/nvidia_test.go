package apt

import (
	"os"
	"strings"
	"testing"
)

func TestNVIDIASourceScopesTrustAndPreservesOtherRepositories(t *testing.T) {
	for _, tc := range []struct{ path, before, after string }{
		{"vendor.list", "# keep\ndeb [arch=amd64 signed-by=/old/key.gpg] " + nvidiaRepositoryURL + "/amd64 /\ndeb https://download.docker.com/linux/ubuntu jammy stable\n", "# keep\ndeb [arch=amd64 signed-by=" + nvidiaKeyPath + "] " + nvidiaRepositoryURL + "/amd64 /\ndeb https://download.docker.com/linux/ubuntu jammy stable\n"},
		{"vendor.list", "deb " + nvidiaRepositoryURL + "/$(ARCH) /\n", "deb [signed-by=" + nvidiaKeyPath + "] " + nvidiaRepositoryURL + "/$(ARCH) /\n"},
		{"vendor.sources", "Types: deb\nURIs: " + nvidiaRepositoryURL + "/amd64\nSuites: /\nSigned-By: /old/key.gpg\n\nTypes: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: jammy\nComponents: stable\n", "Types: deb\nURIs: " + nvidiaRepositoryURL + "/amd64\nSuites: /\nSigned-By: " + nvidiaKeyPath + "\n\nTypes: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: jammy\nComponents: stable\n"},
		{"vendor.sources", "Types: deb\nURIs: " + nvidiaRepositoryURL + "/amd64\nSuites: /\n", "Types: deb\nURIs: " + nvidiaRepositoryURL + "/amd64\nSuites: /\nSigned-By: " + nvidiaKeyPath + "\n"},
		{"disabled.list", "# deb " + nvidiaRepositoryURL + "/amd64 /\n", "# deb " + nvidiaRepositoryURL + "/amd64 /\n"},
		{"disabled.sources", "Enabled: no\nTypes: deb\nURIs: " + nvidiaRepositoryURL + "/amd64\nSuites: /\n", "Enabled: no\nTypes: deb\nURIs: " + nvidiaRepositoryURL + "/amd64\nSuites: /\n"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got, changed, active, err := nvidiaSource(tc.before, tc.path)
			if err != nil || got != tc.after || changed != (tc.before != tc.after) {
				t.Fatalf("got %q, changed=%t, error=%v; want %q", got, changed, err, tc.after)
			}
			if active == strings.HasPrefix(tc.path, "disabled") {
				t.Fatalf("active source detection failed: %q", tc.path)
			}
			if _, changed, _, err := nvidiaSource(got, tc.path); err != nil || changed {
				t.Fatalf("rewrite is not idempotent: changed=%t, error=%v", changed, err)
			}
		})
	}
	if _, _, _, err := nvidiaSource("Types: deb\nURIs: "+nvidiaRepositoryURL+"/amd64 https://example.org\nSuites: /\n", "mixed.sources"); err == nil {
		t.Fatal("mixed vendor stanza accepted")
	}
}

func TestCheckRejectsStaleIndexesEvenWithZeroExit(t *testing.T) {
	for _, text := range []string{
		"W: Failed to fetch https://unknown.invalid/InRelease NO_PUBKEY UNKNOWN\nW: Some index files failed to download.",
		"Err:1 https://unknown.invalid Release\n", "E: Conflicting values set for option Signed-By",
	} {
		service := Service{Run: func(Command) (Result, error) { return Result{Output: text}, nil }}
		if diagnosis, err := service.Check(); err == nil || diagnosis.Healthy {
			t.Fatalf("stale index was accepted: %#v, %v", diagnosis, err)
		}
	}
}

func TestEnsureRepairsNVIDIAZeroExitSigningFailureAndRollsBack(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		t.Run(map[bool]string{true: "success", false: "rollback"}[healthy], func(t *testing.T) {
			updates, installs, restores := 0, 0, 0
			service := Service{Fetch: func(url string) ([]byte, error) {
				if url != nvidiaKeyURL {
					t.Fatalf("unexpected key URL %s", url)
				}
				return []byte("official key fixture"), nil
			}, Run: func(command Command) (Result, error) {
				switch command.Name {
				case "apt-get":
					updates++
					if updates > 1 && healthy {
						return Result{}, nil
					}
					return Result{Output: "Err:1 " + nvidiaRepositoryURL + "/amd64 InRelease\nNO_PUBKEY DDCAE044F796ECB0\n"}, nil
				case "find":
					return Result{Output: "/etc/apt/sources.list.d/nvidia-container-toolkit.list\n"}, nil
				case "cat":
					return Result{Output: "deb " + nvidiaRepositoryURL + "/amd64 /\n"}, nil
				case "stat":
					return Result{Output: "600\n"}, nil
				case "test":
					if strings.HasSuffix(command.Args[1], ".list") {
						return Result{}, nil
					}
					return Result{ExitCode: 1}, nil
				case "gpg":
					return Result{Output: "pub::::::::::\nfpr:::::::::" + nvidiaFingerprint + ":\nsub::::::::::\nfpr:::::::::SUBKEY:\n"}, nil
				case "install":
					if len(command.Args) > 3 && command.Args[0] == "-m" {
						installs++
						if strings.HasSuffix(command.Args[len(command.Args)-1], ".list") {
							if command.Args[1] != "600" {
								t.Fatal("source permissions widened")
							}
							data, err := os.ReadFile(command.Args[2])
							if err != nil || !strings.Contains(string(data), "signed-by="+nvidiaKeyPath) {
								t.Fatal("source was not scoped")
							}
						}
					}
				case "cp", "rm":
					restores++
				default:
					t.Fatalf("unexpected command %#v", command)
				}
				return Result{}, nil
			}}
			changed, err := service.Ensure(nil)
			if changed != healthy || (err == nil) != healthy || updates != 2 || installs != 2 {
				t.Fatalf("changed=%t err=%v updates=%d installs=%d", changed, err, updates, installs)
			}
			if !healthy && (restores < 2 || !strings.Contains(err.Error(), "were restored")) {
				t.Fatal("failed repair was not restored")
			}
		})
	}
}

func TestFingerprintRejectsExtraPrimaryKeys(t *testing.T) {
	service := Service{Run: func(Command) (Result, error) {
		return Result{Output: "pub::::::::::\nfpr:::::::::" + nvidiaFingerprint + ":\npub::::::::::\nfpr:::::::::UNEXPECTED:\n"}, nil
	}}
	if err := service.verifyRepositoryFingerprint("/staged", "/staged/key", nvidiaFingerprint, "NVIDIA"); err == nil {
		t.Fatal("keyring with an unexpected additional primary key accepted")
	}
}
