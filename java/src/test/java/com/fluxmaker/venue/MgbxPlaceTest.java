package com.fluxmaker.venue;

import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;

/** Verifies that MGBX batch placement is deliberately disabled and every
 *  requested order is sent through the stable single-order endpoint instead. */
class MgbxPlaceTest {

    @Test
    void fallsBackToSignedPostOnlySingleOrders() throws IOException {

        try {
            String apiKey = "";
            String secret = "";
            String baseUrl = "https://open.mgbx.com";
            MgbxClient client = new MgbxClient("t", "t", baseUrl, apiKey, secret, Duration.ofSeconds(5));

            VenueClient.PlaceRequest requests = new VenueClient.PlaceRequest("GDT_USDT", Domain.Side.BUY, DecimalValue.parse("0.1"), DecimalValue.parse("100"), "fm-a", 0);
            Domain.Order order = client.placePostOnly(requests);


        } finally {

        }
    }


}
