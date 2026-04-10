package vmm

type DriveConfig struct {
	ID          string             `json:"id,omitempty"`
	Path        string             `json:"path,omitempty"`
	Root        bool               `json:"root,omitempty"`
	ReadOnly    bool               `json:"read_only,omitempty"`
	RateLimiter *RateLimiterConfig `json:"rate_limiter,omitempty"`
}

func cloneDriveConfigs(src []DriveConfig) []DriveConfig {
	if len(src) == 0 {
		return nil
	}
	dst := make([]DriveConfig, 0, len(src))
	for _, drive := range src {
		cloned := drive
		cloned.RateLimiter = cloneRateLimiterConfig(drive.RateLimiter)
		dst = append(dst, cloned)
	}
	return dst
}

func (cfg Config) DriveList() []DriveConfig {
	if len(cfg.Drives) > 0 {
		return cloneDriveConfigs(cfg.Drives)
	}
	if cfg.DiskImage == "" {
		return nil
	}
	return []DriveConfig{{
		ID:          "root",
		Path:        cfg.DiskImage,
		Root:        true,
		ReadOnly:    cfg.DiskRO,
		RateLimiter: cloneRateLimiterConfig(cfg.BlockRateLimiter),
	}}
}

func (cfg Config) RootDrive() (DriveConfig, bool) {
	for _, drive := range cfg.DriveList() {
		if drive.Root {
			return drive, true
		}
	}
	return DriveConfig{}, false
}

func (cfg Config) HasAdditionalDrives() bool {
	rootSeen := false
	for _, drive := range cfg.DriveList() {
		if drive.Root {
			if rootSeen {
				return true
			}
			rootSeen = true
			continue
		}
		return true
	}
	return false
}
