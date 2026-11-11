package httpapi

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/service"
)

func (a *API) createMigration(c *gin.Context) {
	m, ok := a.mutation(c, false)
	if !ok {
		return
	}
	var req service.MigrationRequest
	if !decode(c, &req) {
		return
	}
	v, e := a.service.CreateMigration(c.Request.Context(), m, req)
	respond(c, v, e)
}
func (a *API) getMigration(c *gin.Context) {
	q, ok := query(c)
	if !ok {
		return
	}
	v, e := a.service.GetMigration(c.Request.Context(), c.Param("id"), q)
	if e == nil {
		c.Header("ETag", "\""+fmt.Sprint(v["revision"])+"\"")
	}
	getResult(c, v, e)
}
func (a *API) cancelMigration(c *gin.Context) {
	m, ok := a.mutation(c, true)
	if !ok {
		return
	}
	var req core.ReasonRequest
	if !decode(c, &req) {
		return
	}
	v, e := a.service.CancelMigration(c.Request.Context(), m, c.Param("id"), req)
	respond(c, v, e)
}
