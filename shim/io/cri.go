package io

// from https://github.com/kubernetes/cri-api/blob/87ee4e17aba6baf0bfd719ec0532af4000e83c5d/pkg/apis/runtime/v1/constants.go

// LogTag is the tag of a log line in CRI container log.
// Currently defined log tags:
// * First tag: Partial/Full - P/F.
// The field in the container log format can be extended to include multiple
// tags by using a delimiter, but changes should be rare. If it becomes clear
// that better extensibility is desired, a more extensible format (e.g., json)
// should be adopted as a replacement and/or addition.
type LogTag string

const (
	// LogTagPartial means the line is part of multiple lines.
	LogTagPartial LogTag = "P"
	// LogTagFull means the line is a single full line or the end of multiple lines.
	LogTagFull LogTag = "F"
)
