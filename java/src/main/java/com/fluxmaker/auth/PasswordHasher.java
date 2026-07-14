package com.fluxmaker.auth;

import org.bouncycastle.crypto.generators.Argon2BytesGenerator;
import org.bouncycastle.crypto.params.Argon2Parameters;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.SecureRandom;
import java.util.Base64;

public final class PasswordHasher {
    private static final int ITERATIONS = 3;
    private static final int MEMORY_KB = 64 * 1024;
    private static final int PARALLELISM = 2;
    private static final int KEY_LENGTH = 32;
    private static final SecureRandom RANDOM = new SecureRandom();

    private PasswordHasher() {}

    public static String hash(String password) {
        byte[] input = password.getBytes(StandardCharsets.UTF_8);
        if (input.length < 12) throw new IllegalArgumentException("password must contain at least 12 characters");
        byte[] salt = new byte[16];
        RANDOM.nextBytes(salt);
        byte[] result = derive(input, salt, ITERATIONS, MEMORY_KB, PARALLELISM, KEY_LENGTH);
        Base64.Encoder base64 = Base64.getEncoder().withoutPadding();
        return "$argon2id$v=19$m=" + MEMORY_KB + ",t=" + ITERATIONS + ",p=" + PARALLELISM + "$" + base64.encodeToString(salt) + "$" + base64.encodeToString(result);
    }

    public static boolean verify(String encoded, String password) {
        try {
            String[] parts = encoded.split("\\$", -1);
            if (parts.length != 6 || !parts[1].equals("argon2id") || !parts[2].equals("v=19")) return false;
            String[] parameters = parts[3].split(",");
            int memory = Integer.parseInt(parameters[0].substring(2));
            int iterations = Integer.parseInt(parameters[1].substring(2));
            int parallelism = Integer.parseInt(parameters[2].substring(2));
            Base64.Decoder base64 = Base64.getDecoder();
            byte[] salt = base64.decode(parts[4]);
            byte[] expected = base64.decode(parts[5]);
            byte[] actual = derive(password.getBytes(StandardCharsets.UTF_8), salt, iterations, memory, parallelism, expected.length);
            return MessageDigest.isEqual(expected, actual);
        } catch (RuntimeException ignored) { return false; }
    }

    private static byte[] derive(byte[] password, byte[] salt, int iterations, int memory, int parallelism, int length) {
        Argon2Parameters parameters = new Argon2Parameters.Builder(Argon2Parameters.ARGON2_id)
                .withVersion(Argon2Parameters.ARGON2_VERSION_13).withSalt(salt).withIterations(iterations)
                .withMemoryAsKB(memory).withParallelism(parallelism).build();
        Argon2BytesGenerator generator = new Argon2BytesGenerator();
        generator.init(parameters);
        byte[] result = new byte[length];
        generator.generateBytes(password, result);
        return result;
    }
}
