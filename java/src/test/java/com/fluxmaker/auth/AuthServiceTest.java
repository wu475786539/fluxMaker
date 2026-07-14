package com.fluxmaker.auth;

import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;

class AuthServiceTest {
    @Test void hashesTokensLikeGo() {
        assertEquals("fluxmaker:session:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", AuthService.sessionKey("abc"));
    }
}
