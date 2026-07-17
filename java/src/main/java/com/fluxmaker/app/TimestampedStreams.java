package com.fluxmaker.app;

import java.io.OutputStream;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.time.Instant;

/** Adds one UTC ISO-8601 timestamp to every application stdout/stderr line. */
final class TimestampedStreams {
    private static boolean installed;

    private TimestampedStreams() {}

    static synchronized void install() {
        if (installed) return;
        System.setOut(timestamped(System.out));
        System.setErr(timestamped(System.err));
        installed = true;
    }

    static PrintStream timestamped(PrintStream destination) {
        return new PrintStream(new TimestampedOutputStream(destination), true, StandardCharsets.UTF_8);
    }

    private static final class TimestampedOutputStream extends OutputStream {
        private final PrintStream destination;
        private boolean lineStart = true;

        private TimestampedOutputStream(PrintStream destination) {
            this.destination = destination;
        }

        @Override
        public synchronized void write(int value) {
            if (lineStart && value != '\n' && value != '\r') {
                destination.print(Instant.now());
                destination.print(' ');
                lineStart = false;
            }
            destination.write(value);
            if (value == '\n') lineStart = true;
        }

        @Override
        public synchronized void write(byte[] values, int offset, int length) {
            for (int index = offset; index < offset + length; index++) write(values[index] & 0xff);
        }

        @Override
        public synchronized void flush() {
            destination.flush();
        }
    }
}
