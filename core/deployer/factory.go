package deployer

// Factory creates deployer instances
type Factory struct {
	basePath string
}

func NewFactory(basePath string) *Factory {
	return &Factory{basePath: basePath}
}

// BasePath exposes the configured base path (used by non-Docker deployers).
func (f *Factory) BasePath() string {
	return f.basePath
}

func (f *Factory) Deployer() *DockerDeployer {
	return NewDockerDeployer(f.basePath)
}
