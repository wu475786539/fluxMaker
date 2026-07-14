package com.fluxmaker.credentials;

import org.junit.jupiter.api.Test;

import java.nio.charset.StandardCharsets;
import java.util.Base64;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;

class CredentialServiceTest {
    @Test void usesGoCompatibleNonceCiphertextLayoutAndAad() {
        String key = Base64.getEncoder().encodeToString("0123456789abcdef0123456789abcdef".getBytes(StandardCharsets.UTF_8));
        CredentialService service = new CredentialService(null, key);
        byte[] nonce = new byte[12];
        for (int index = 0; index < nonce.length; index++) nonce[index] = (byte) index;
        byte[] encrypted = service.encrypt("secret", "binance:api-secret", nonce);
        assertEquals(12 + 6 + 16, encrypted.length);
        assertEquals("secret", service.decrypt(encrypted, "binance:api-secret"));
        assertThrows(IllegalArgumentException.class, () -> service.decrypt(encrypted, "mgbx:api-secret"));
    }
}
