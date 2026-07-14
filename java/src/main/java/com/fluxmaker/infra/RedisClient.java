package com.fluxmaker.infra;

import java.io.BufferedInputStream;
import java.io.BufferedOutputStream;
import java.io.ByteArrayOutputStream;
import java.io.EOFException;
import java.io.IOException;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/** Minimal RESP2 client covering the exact Redis commands used by FluxMaker. */
public final class RedisClient implements AutoCloseable {
    private final String host;
    private final int port;
    private final String password;
    private final int database;
    private final int timeoutMs;

    public RedisClient(String address, String password, int database) {
        if (address == null || address.isBlank()) throw new IllegalArgumentException("REDIS_ADDR is required");
        int separator = address.lastIndexOf(':');
        this.host = separator < 0 ? address : address.substring(0, separator);
        this.port = separator < 0 ? 6379 : Integer.parseInt(address.substring(separator + 1));
        this.password = password == null ? "" : password;
        this.database = database;
        this.timeoutMs = 10_000;
    }

    public static RedisClient fromEnv() {
        String rawDb = System.getenv().getOrDefault("REDIS_DB", "0");
        return new RedisClient(System.getenv("REDIS_ADDR"), System.getenv("REDIS_PASSWORD"), Integer.parseInt(rawDb));
    }

    public void ping() { command("PING"); }

    public byte[] get(String key) {
        Object value = command(bytes("GET"), bytes(key));
        return value instanceof byte[] data ? data : null;
    }

    public String getString(String key) {
        byte[] value = get(key);
        return value == null ? null : new String(value, StandardCharsets.UTF_8);
    }

    public void set(String key, byte[] value, Duration ttl) {
        if (ttl == null || ttl.isZero() || ttl.isNegative()) command(bytes("SET"), bytes(key), value);
        else command(bytes("SET"), bytes(key), value, bytes("PX"), bytes(Long.toString(ttl.toMillis())));
    }

    public void set(String key, String value, Duration ttl) { set(key, bytes(value), ttl); }
    public long delete(String... keys) { return number(command(join("DEL", keys))); }
    public long increment(String key) { return number(command("INCR", key)); }
    public long hset(String key, String field, byte[] value) { return number(command(bytes("HSET"), bytes(key), bytes(field), value)); }
    public long hdel(String key, String field) { return number(command("HDEL", key, field)); }

    public Map<String, byte[]> hgetall(String key) {
        List<Object> values = array(command("HGETALL", key));
        Map<String, byte[]> result = new LinkedHashMap<>();
        for (int index = 0; index + 1 < values.size(); index += 2) {
            result.put(text(values.get(index)), (byte[]) values.get(index + 1));
        }
        return result;
    }

    public long lpush(String key, byte[] value) { return number(command(bytes("LPUSH"), bytes(key), value)); }
    public void ltrim(String key, long start, long stop) { command("LTRIM", key, Long.toString(start), Long.toString(stop)); }
    public void expire(String key, Duration ttl) { command("PEXPIRE", key, Long.toString(ttl.toMillis())); }

    public List<byte[]> lrange(String key, long start, long stop) {
        List<Object> values = array(command("LRANGE", key, Long.toString(start), Long.toString(stop)));
        List<byte[]> result = new ArrayList<>(values.size());
        for (Object value : values) result.add((byte[]) value);
        return result;
    }

    public long evalLong(String script, List<String> keys, String... args) {
        List<byte[]> command = new ArrayList<>();
        command.add(bytes("EVAL"));
        command.add(bytes(script));
        command.add(bytes(Integer.toString(keys.size())));
        for (String key : keys) command.add(bytes(key));
        for (String argument : args) command.add(bytes(argument));
        return number(command(command.toArray(byte[][]::new)));
    }

    public void xaddPayload(String stream, int maxLength, byte[] payload) {
        command(bytes("XADD"), bytes(stream), bytes("MAXLEN"), bytes("~"), bytes(Integer.toString(maxLength)), bytes("*"), bytes("payload"), payload);
    }

    public Object command(String... args) {
        byte[][] values = new byte[args.length][];
        for (int index = 0; index < args.length; index++) values[index] = bytes(args[index]);
        return command(values);
    }

    public Object command(byte[]... args) {
        try (Socket socket = new Socket()) {
            socket.connect(new InetSocketAddress(host, port), timeoutMs);
            socket.setSoTimeout(timeoutMs);
            BufferedInputStream input = new BufferedInputStream(socket.getInputStream());
            BufferedOutputStream output = new BufferedOutputStream(socket.getOutputStream());
            if (!password.isEmpty()) {
                write(output, bytes("AUTH"), bytes(password));
                checkOk(read(input));
            }
            if (database != 0) {
                write(output, bytes("SELECT"), bytes(Integer.toString(database)));
                checkOk(read(input));
            }
            write(output, args);
            return read(input);
        } catch (IOException e) {
            throw new IllegalStateException("redis command failed: " + e.getMessage(), e);
        }
    }

    private static void write(BufferedOutputStream output, byte[]... args) throws IOException {
        output.write(bytes("*" + args.length + "\r\n"));
        for (byte[] argument : args) {
            output.write(bytes("$" + argument.length + "\r\n"));
            output.write(argument);
            output.write(bytes("\r\n"));
        }
        output.flush();
    }

    private static Object read(BufferedInputStream input) throws IOException {
        int marker = input.read();
        if (marker < 0) throw new EOFException("redis closed connection");
        return switch (marker) {
            case '+' -> line(input);
            case '-' -> throw new RedisException(line(input));
            case ':' -> Long.parseLong(line(input));
            case '$' -> bulk(input);
            case '*' -> multi(input);
            default -> throw new IOException("invalid RESP marker " + marker);
        };
    }

    private static byte[] bulk(BufferedInputStream input) throws IOException {
        int length = Integer.parseInt(line(input));
        if (length < 0) return null;
        byte[] result = input.readNBytes(length);
        if (result.length != length) throw new EOFException("short Redis bulk response");
        requireCrlf(input);
        return result;
    }

    private static List<Object> multi(BufferedInputStream input) throws IOException {
        int length = Integer.parseInt(line(input));
        if (length < 0) return List.of();
        List<Object> values = new ArrayList<>(length);
        for (int index = 0; index < length; index++) values.add(read(input));
        return values;
    }

    private static String line(BufferedInputStream input) throws IOException {
        ByteArrayOutputStream result = new ByteArrayOutputStream();
        int previous = -1;
        while (true) {
            int current = input.read();
            if (current < 0) throw new EOFException("short Redis response");
            if (previous == '\r' && current == '\n') break;
            if (previous >= 0) result.write(previous);
            previous = current;
        }
        return result.toString(StandardCharsets.UTF_8);
    }

    private static void requireCrlf(BufferedInputStream input) throws IOException {
        if (input.read() != '\r' || input.read() != '\n') throw new IOException("invalid Redis bulk terminator");
    }

    private static byte[][] join(String first, String[] rest) {
        byte[][] result = new byte[rest.length + 1][];
        result[0] = bytes(first);
        for (int index = 0; index < rest.length; index++) result[index + 1] = bytes(rest[index]);
        return result;
    }

    private static void checkOk(Object value) {
        if (!"OK".equals(value)) throw new RedisException("expected OK, got " + value);
    }

    @SuppressWarnings("unchecked")
    private static List<Object> array(Object value) { return (List<Object>) value; }
    private static long number(Object value) { return value instanceof Number number ? number.longValue() : Long.parseLong(text(value)); }
    private static String text(Object value) { return value instanceof byte[] data ? new String(data, StandardCharsets.UTF_8) : String.valueOf(value); }
    private static byte[] bytes(String value) { return value.getBytes(StandardCharsets.UTF_8); }

    @Override public void close() { /* connections are command-scoped */ }

    public static final class RedisException extends RuntimeException {
        public RedisException(String message) { super(message); }
    }
}
