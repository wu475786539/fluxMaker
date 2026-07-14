package com.fluxmaker.app;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.FileTime;
import java.time.Duration;
import java.time.Instant;

public final class HeartbeatFiles {
    private HeartbeatFiles() {}

    public static void touch(String rawPath) {
        if (rawPath == null || rawPath.isBlank()) return;
        Path path = Path.of(rawPath);
        try {
            Path parent = path.getParent(); if (parent != null) Files.createDirectories(parent);
            if (Files.exists(path)) Files.setLastModifiedTime(path, FileTime.from(Instant.now()));
            else Files.createFile(path);
        } catch (IOException e) { throw new IllegalStateException("touch heartbeat " + path, e); }
    }

    public static Duration age(String rawPath) {
        if (rawPath == null || rawPath.isBlank()) throw new IllegalArgumentException("heartbeat path is empty");
        try { return Duration.between(Files.getLastModifiedTime(Path.of(rawPath)).toInstant(), Instant.now()); }
        catch (IOException e) { throw new IllegalStateException("read heartbeat " + rawPath, e); }
    }
}
