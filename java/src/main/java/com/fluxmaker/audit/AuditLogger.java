package com.fluxmaker.audit;

import com.fluxmaker.json.Json;

import java.io.BufferedWriter;
import java.io.IOException;
import java.nio.channels.FileChannel;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.StandardOpenOption;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

public final class AuditLogger {
    private final Path path;
    private final long maxBytes;
    private final int backups;
    private final List<byte[]> pending = new ArrayList<>();

    public AuditLogger(String path, long maxBytes, int backups) { this.path = path == null || path.isEmpty() ? null : Path.of(path); this.maxBytes = maxBytes; this.backups = backups; }
    public synchronized void record(String type, Object data) { if (path != null) pending.add(Json.writeBytes(Map.of("timestamp", Instant.now(), "type", type, "data", data))); }
    public synchronized int pendingCount() { return pending.size(); }

    public synchronized void flush() {
        if (path == null || pending.isEmpty()) return;
        try {
            Files.createDirectories(path.getParent()); long incoming = pending.stream().mapToLong(value -> value.length + 1L).sum(); rotate(incoming);
            try (FileChannel channel = FileChannel.open(path, StandardOpenOption.CREATE, StandardOpenOption.APPEND, StandardOpenOption.WRITE)) {
                for (byte[] item : pending) { channel.write(java.nio.ByteBuffer.wrap(item)); channel.write(java.nio.ByteBuffer.wrap(new byte[]{'\n'})); }
                channel.force(true);
            }
            pending.clear();
        } catch (IOException e) { throw new IllegalStateException("flush audit: " + e.getMessage(), e); }
    }

    private void rotate(long incoming) throws IOException {
        if (maxBytes <= 0 || backups <= 0 || !Files.exists(path) || Files.size(path) == 0 || Files.size(path) + incoming <= maxBytes) return;
        Files.deleteIfExists(Path.of(path + "." + backups));
        for (int index = backups - 1; index >= 1; index--) { Path old = Path.of(path + "." + index); if (Files.exists(old)) Files.move(old, Path.of(path + "." + (index + 1)), StandardCopyOption.REPLACE_EXISTING); }
        Files.move(path, Path.of(path + ".1"), StandardCopyOption.REPLACE_EXISTING);
    }
}
