# sarif-to-codequality

![publish-docker-image](https://github.com/nakatanakatana/mytools/actions/workflows/publish-docker-image.yaml/badge.svg)
![CI](https://github.com/nakatanakatana/mytools/actions/workflows/ci.yaml/badge.svg)
![Coverage](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/coverage.svg?raw=true)
![Code to Test Ratio](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/ratio.svg?raw=true)
![Test Execution Time](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/time.svg?raw=true)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/nakatanakatana/mytools)

A CLI tool that converts SARIF (Static Analysis Results Interchange Format) files into GitLab Code Quality format.

## Running

```bash
# Build the binary
go build -o dist/sarif-to-codequality ./cmd/sarif-to-codequality

# Convert SARIF format to GitLab Code Quality format
./dist/sarif-to-codequality -input test.sarif -output gl-code-quality-report.json
```
