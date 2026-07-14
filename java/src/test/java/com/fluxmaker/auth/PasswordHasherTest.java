package com.fluxmaker.auth;

import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class PasswordHasherTest {
    @Test void hashesAndVerifiesGoCompatibleArgon2id() {
        String hash = PasswordHasher.hash("correct horse battery staple");
        assertTrue(hash.startsWith("$argon2id$v=19$m=65536,t=3,p=2$"));
        assertTrue(PasswordHasher.verify(hash, "correct horse battery staple"));
        assertFalse(PasswordHasher.verify(hash, "wrong password"));
    }
    @Test void enforcesMinimum() { assertThrows(IllegalArgumentException.class, () -> PasswordHasher.hash("short")); }
}
