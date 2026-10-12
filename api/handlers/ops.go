package handlers

import "gorm.io/gorm"

type OpsHandler struct {
	db       *gorm.DB
	basePath string
}

func NewOpsHandler(db *gorm.DB, basePath string) *OpsHandler {
	return &OpsHandler{db: db, basePath: basePath}
}
