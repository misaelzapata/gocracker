package usercfg

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Resolved struct {
	UID    uint32
	GID    uint32
	Groups []uint32
	Home   string
	Name   string
}

type passwdEntry struct {
	Name string
	UID  uint32
	GID  uint32
	Home string
}

type groupEntry struct {
	Name    string
	GID     uint32
	Members []string
}

func Resolve(rootfs, spec string) (Resolved, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Resolved{}, fmt.Errorf("empty user spec")
	}

	passwdEntries, err := readPasswd(filepath.Join(rootfs, "etc", "passwd"))
	if err != nil {
		return Resolved{}, err
	}
	groupEntries, err := readGroups(filepath.Join(rootfs, "etc", "group"))
	if err != nil {
		return Resolved{}, err
	}

	userPart := spec
	groupPart := ""
	if parts := strings.SplitN(spec, ":", 2); len(parts) == 2 {
		userPart = parts[0]
		groupPart = parts[1]
	}
	if userPart == "" {
		return Resolved{}, fmt.Errorf("invalid user spec %q", spec)
	}

	resolved := Resolved{}
	userEntry, hasUserEntry, err := resolveUser(passwdEntries, userPart)
	if err != nil {
		return Resolved{}, err
	}
	resolved.UID = userEntry.UID
	resolved.GID = userEntry.GID
	resolved.Home = userEntry.Home
	resolved.Name = userEntry.Name

	if groupPart != "" {
		gid, err := resolveGroup(groupEntries, groupPart)
		if err != nil {
			return Resolved{}, err
		}
		resolved.GID = gid
		return resolved, nil
	}

	if !hasUserEntry {
		resolved.GID = resolved.UID
		return resolved, nil
	}

	if resolved.Name != "" {
		resolved.Groups = supplementalGroups(groupEntries, resolved.Name, resolved.GID)
	}
	return resolved, nil
}

func resolveUser(passwdEntries []passwdEntry, value string) (passwdEntry, bool, error) {
	if uid, err := parseUint32(value); err == nil {
		for _, entry := range passwdEntries {
			if entry.UID == uid {
				return entry, true, nil
			}
		}
		return passwdEntry{UID: uid, GID: uid}, false, nil
	}
	for _, entry := range passwdEntries {
		if entry.Name == value {
			return entry, true, nil
		}
	}
	return passwdEntry{}, false, fmt.Errorf("unknown user %q", value)
}

func resolveGroup(groupEntries []groupEntry, value string) (uint32, error) {
	if gid, err := parseUint32(value); err == nil {
		return gid, nil
	}
	for _, entry := range groupEntries {
		if entry.Name == value {
			return entry.GID, nil
		}
	}
	return 0, fmt.Errorf("unknown group %q", value)
}

func supplementalGroups(groupEntries []groupEntry, username string, primaryGID uint32) []uint32 {
	var groups []uint32
	for _, entry := range groupEntries {
		if entry.GID == primaryGID {
			continue
		}
		for _, member := range entry.Members {
			if member == username {
				groups = append(groups, entry.GID)
				break
			}
		}
	}
	return groups
}

func readPasswd(path string) ([]passwdEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var entries []passwdEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 7 {
			continue
		}
		uid, err := parseUint32(parts[2])
		if err != nil {
			return nil, fmt.Errorf("parse uid in %s: %w", path, err)
		}
		gid, err := parseUint32(parts[3])
		if err != nil {
			return nil, fmt.Errorf("parse gid in %s: %w", path, err)
		}
		entries = append(entries, passwdEntry{
			Name: parts[0],
			UID:  uid,
			GID:  gid,
			Home: parts[5],
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func readGroups(path string) ([]groupEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var entries []groupEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 4 {
			continue
		}
		gid, err := parseUint32(parts[2])
		if err != nil {
			return nil, fmt.Errorf("parse gid in %s: %w", path, err)
		}
		members := []string{}
		if parts[3] != "" {
			members = strings.Split(parts[3], ",")
		}
		entries = append(entries, groupEntry{
			Name:    parts[0],
			GID:     gid,
			Members: members,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func parseUint32(value string) (uint32, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}
