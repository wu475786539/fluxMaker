package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.venue.VenueClient;

import java.time.Instant;
import java.util.List;

/**
 * Pure extension point for internal volume simulation algorithms.
 *
 * <p>The planner receives market data, configuration, and an optional
 * venue client. When {@code venueClient} is non-null the implementation
 * may place real orders on the venue; when null it should generate
 * simulated plans only.
 *
 * <p>When {@link EventPlan#orderId} is non-null the plan represents a real
 * order that has already been placed on a venue; {@link TradeSimulator}
 * will materialize it as a non-simulated fill.
 */
@FunctionalInterface
public interface VolumeSimulationPlanner {
    List<EventPlan> plan(Request request);

    record Request(
            AppConfig.InstrumentConfig instrument,
            String sourceVenue,
            AppConfig.VenueMarketConfig market,
            Domain.Book book,
            Instant now,
            long sequence,
            VenueClient venueClient
    ) {}

    record EventPlan(Domain.Side side, DecimalValue price, DecimalValue quantity, String orderId) {
        public boolean isReal() { return orderId != null && !orderId.isEmpty(); }
    }
}
