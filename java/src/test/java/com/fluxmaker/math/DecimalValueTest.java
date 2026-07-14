package com.fluxmaker.math;

import com.fluxmaker.json.Json;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;

class DecimalValueTest {
    @Test
    void exactArithmeticAndGoCompatibleJson() {
        DecimalValue value = DecimalValue.parse("0.1").add(DecimalValue.parse("0.2"));
        assertEquals("0.3", value.toString());
        assertEquals("\"0.3\"", Json.write(value));
        assertEquals(value, Json.read("\"0.3\"", DecimalValue.class));
        assertEquals(value, Json.read("0.3", DecimalValue.class));
        assertEquals("0.333333333333333333", DecimalValue.ONE.divide(DecimalValue.of(3)).toString());
    }

    @Test
    void divisionStaysExactRational() {
        // 1/3 is held as an exact fraction, so (1/3)*3 == 1 with no rounding drift.
        // A BigDecimal-scaled implementation would fail this.
        assertEquals(DecimalValue.ONE, DecimalValue.ONE.divide(DecimalValue.of(3)).multiply(DecimalValue.of(3)));
        // withinBps cross-multiply boundary is exact: |100.5-100|*10000 == 100*50.
        assertEquals(0, DecimalValue.parse("100.5").subtract(DecimalValue.parse("100")).abs()
                .multiply(DecimalValue.TEN_THOUSAND)
                .compareTo(DecimalValue.parse("100").multiply(DecimalValue.of(50))));
    }

    @Test
    void quantizesExactly() {
        DecimalValue value = DecimalValue.parse("1.239");
        DecimalValue step = DecimalValue.parse("0.01");
        assertEquals("1.23", value.quantizeDown(step).toString());
        assertEquals("1.24", value.quantizeUp(step).toString());
        assertThrows(IllegalArgumentException.class, () -> value.quantizeDown(DecimalValue.ZERO));
    }
}
