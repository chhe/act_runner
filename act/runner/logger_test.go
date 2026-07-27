// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"io"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValueMasker(t *testing.T) {
	table := []struct {
		name       string
		lines      string
		secrets    map[string]string
		masks      []string
		disallowed []string
	}{
		{
			name:  "Multiline Private Key",
			lines: "cat << EOF > private.key\nPRIVATE_KEY_BEGIN\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\nPRIVATE_KEY_END\nEOF",
			secrets: map[string]string{
				"PRIVATE_KEY": "PRIVATE_KEY_BEGIN\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\nPRIVATE_KEY_END",
			},
			disallowed: []string{"KEY", "dsdfseffefsefes", "PRIVATE_KEY_END"},
		},
		{
			name:       "Multiline Private Key in masks",
			lines:      "cat << EOF > private.key\nPRIVATE_KEY_BEGIN\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\nPRIVATE_KEY_END\nEOF",
			masks:      []string{"PRIVATE_KEY_BEGIN\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\ndsdfseffefsefes\nPRIVATE_KEY_END"},
			disallowed: []string{"KEY", "dsdfseffefsefes", "PRIVATE_KEY_END"},
		},
		{
			name:       "Secret containing a percent sign",
			lines:      "##[error]login failed for pass%25word",
			secrets:    map[string]string{"TOKEN": "pass%word"},
			disallowed: []string{"pass%25word"},
		},
	}
	for _, entry := range table {
		t.Run(entry.name, func(t *testing.T) {
			ctx := WithMasks(t.Context(), &entry.masks)
			masker := valueMasker(false, entry.secrets)
			for line := range strings.SplitSeq(entry.lines, "\n") {
				lentry := masker(&logrus.Entry{
					Context: ctx,
					Message: line,
				})
				for _, line := range entry.disallowed {
					assert.NotContains(t, lentry.Message, line)
				}
			}
		})
	}
}

func TestJobLogFormatterDecodesCommandData(t *testing.T) {
	logger := logrus.New()
	logger.Out = io.Discard
	format := func(message string) string {
		out, err := (&jobLogFormatter{}).Format(&logrus.Entry{Logger: logger, Message: message, Data: logrus.Fields{rawOutputField: true}})
		require.NoError(t, err)
		return string(out)
	}

	assert.Contains(t, format("##[error]deploy 50%25 traffic"), "##[error]deploy 50% traffic")
	// a plain line is not command data and keeps its literal escapes
	assert.Contains(t, format("progress 50%25 done"), "progress 50%25 done")
}
