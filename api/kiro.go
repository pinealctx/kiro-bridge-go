package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type kiroUsageResponse struct {
	Total       float64 `json:"total"`
	Used        float64 `json:"used"`
	PercentUsed float64 `json:"percent_used"`
}

func (s *Server) handleKiroUsage(c *gin.Context) {
	accessToken, err := s.tm.GetAccessToken(s.cfg.IdcRefreshURL)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": err.Error(), "type": "service_unavailable"}})
		return
	}
	s.client.IsExternalIdP = s.tm.IsExternalIdP

	limits, err := s.client.GetUsageLimits(c.Request.Context(), accessToken)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": err.Error(), "type": "service_unavailable"}})
		return
	}

	total := limits.Usage.LimitPrecise
	if total == 0 {
		total = limits.Usage.Limit
	}
	used := limits.Usage.UsedPrecise
	if used == 0 {
		used = limits.Usage.Used
	}
	percentUsed := limits.Usage.PercentUsed
	if percentUsed == 0 && total > 0 && used > 0 {
		percentUsed = used / total * 100
	}

	c.JSON(http.StatusOK, kiroUsageResponse{
		Total:       total,
		Used:        used,
		PercentUsed: percentUsed,
	})
}
