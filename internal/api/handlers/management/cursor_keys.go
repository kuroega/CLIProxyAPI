package management

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// GetCursorKeys returns configured native Cursor AgentService credentials.
func (h *Handler) GetCursorKeys(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.JSON(200, gin.H{"cursor-api-key": h.cfg.CursorKey})
}

// PutCursorKeys replaces all configured Cursor credentials.
func (h *Handler) PutCursorKeys(c *gin.Context) {
	data, errRead := c.GetRawData()
	if errRead != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var entries []config.CursorKey
	if errDecode := json.Unmarshal(data, &entries); errDecode != nil {
		var body struct {
			Items []config.CursorKey `json:"items"`
		}
		if errItems := json.Unmarshal(data, &body); errItems != nil {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		entries = body.Items
	}
	if entries == nil {
		c.JSON(400, gin.H{"error": "expected an array or an object with an items array"})
		return
	}
	for index := range entries {
		if rejectInvalidCredentialWeight(c, fmt.Sprintf("cursor-api-key[%d].weight", index), entries[index].Weight) {
			return
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.CursorKey = entries
	h.cfg.SanitizeCursorKeys()
	h.persistLocked(c)
}

// DeleteCursorKey removes one Cursor credential by index or API key.
func (h *Handler) DeleteCursorKey(c *gin.Context) {
	indexText := strings.TrimSpace(c.Query("index"))
	apiKey := strings.TrimSpace(c.Query("api-key"))
	h.mu.Lock()
	defer h.mu.Unlock()
	index := -1
	if indexText != "" {
		parsed, errParse := strconv.Atoi(indexText)
		if errParse != nil || parsed < 0 || parsed >= len(h.cfg.CursorKey) {
			c.JSON(404, gin.H{"error": "cursor credential not found"})
			return
		}
		index = parsed
	} else if apiKey != "" {
		for i := range h.cfg.CursorKey {
			if strings.TrimSpace(h.cfg.CursorKey[i].APIKey) == apiKey {
				index = i
				break
			}
		}
	}
	if index < 0 {
		c.JSON(400, gin.H{"error": "specify index or api-key"})
		return
	}
	h.cfg.CursorKey = append(h.cfg.CursorKey[:index], h.cfg.CursorKey[index+1:]...)
	h.cfg.SanitizeCursorKeys()
	h.persistLocked(c)
}
