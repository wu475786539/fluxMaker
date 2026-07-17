package com.fluxmaker.app;

import org.junit.jupiter.api.Test;

import java.io.ByteArrayOutputStream;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.time.Instant;

import static org.junit.jupiter.api.Assertions.assertEquals;

class TimestampedStreamsTest {
    @Test
    void prefixesEveryCompleteLineWithoutSplittingPartialWrites() {
        ByteArrayOutputStream output = new ByteArrayOutputStream();
        PrintStream destination = new PrintStream(output, true, StandardCharsets.UTF_8);
        PrintStream timestamped = TimestampedStreams.timestamped(destination);

        timestamped.print("first");
        timestamped.println(" line");
        timestamped.println("second line");

        String[] lines = output.toString(StandardCharsets.UTF_8).strip().split("\\R");
        assertEquals(2, lines.length);
        assertLogLine(lines[0], "first line");
        assertLogLine(lines[1], "second line");
    }

    private static void assertLogLine(String line, String expectedMessage) {
        int separator = line.indexOf(' ');
        Instant.parse(line.substring(0, separator));
        assertEquals(expectedMessage, line.substring(separator + 1));
    }
}
