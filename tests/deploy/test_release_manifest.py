"""发布包组件清单校验回归（4.0 破坏性规则）。"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "deploy/lib"))

from release_manifest import (  # noqa: E402
    ERROR_AGENT_COMPONENT_MISSING,
    ERROR_COMPONENTS_MISSING,
    ERROR_COMPONENT_DIGEST_INVALID,
    ERROR_COMPONENT_DUPLICATE,
    ERROR_COMPONENT_PATH_INVALID,
    ERROR_CORE_COMPONENT_MISSING,
    ERROR_SCHEMA_INVALID,
    SCHEMA_V2,
    SCHEMA_V3,
    component_summary,
    validate_components,
)


DIGEST = "a" * 64
OTHER = "b" * 64


def document(schema: str = SCHEMA_V3, components: object = None) -> dict:
    payload = {"schema": schema, "version": "4.0.0", "tag": "v4.0.0"}
    if components is not None:
        payload["components"] = components
    return payload


class ValidateComponentsTests(unittest.TestCase):
    def test_v3_package_with_core_and_agent_passes(self):
        components, error = validate_components(
            document(
                components=[
                    {"name": "core", "path": "dbguard", "sha256": DIGEST},
                    {"name": "agent", "path": "agent-app", "sha256": OTHER},
                ]
            )
        )
        self.assertIsNone(error)
        self.assertEqual([component.name for component in components], ["core", "agent"])
        self.assertEqual(component_summary(components), f"core:{DIGEST},agent:{OTHER}")

    def test_v3_package_without_agent_fails_unless_core_only(self):
        only_core = document(components=[{"name": "core", "path": "dbguard", "sha256": DIGEST}])
        components, error = validate_components(only_core)
        self.assertEqual(error, ERROR_AGENT_COMPONENT_MISSING)
        self.assertEqual(components, ())

        components, error = validate_components(only_core, core_only=True)
        self.assertIsNone(error)
        self.assertEqual([component.name for component in components], ["core"])

    def test_v3_package_without_core_fails_even_with_core_only(self):
        only_agent = document(components=[{"name": "agent", "path": "agent-app", "sha256": DIGEST}])
        _, error = validate_components(only_agent, core_only=True)
        self.assertEqual(error, ERROR_CORE_COMPONENT_MISSING)

    def test_v2_package_requires_an_explicit_core_only_declaration(self):
        _, error = validate_components(document(schema=SCHEMA_V2))
        self.assertEqual(error, ERROR_COMPONENTS_MISSING)
        components, error = validate_components(document(schema=SCHEMA_V2), core_only=True)
        self.assertIsNone(error)
        self.assertEqual([component.name for component in components], ["core"])

    def test_unknown_schema_is_rejected(self):
        _, error = validate_components(document(schema="changeguard-release/v9"))
        self.assertEqual(error, ERROR_SCHEMA_INVALID)

    def test_missing_or_empty_component_list_is_rejected(self):
        for value in (None, [], "core", {"name": "core"}):
            with self.subTest(value=value):
                _, error = validate_components(document(components=value))
                self.assertEqual(error, ERROR_COMPONENTS_MISSING)

    def test_duplicate_and_invalid_entries_are_rejected(self):
        cases = {
            ERROR_COMPONENT_DUPLICATE: [
                {"name": "core", "path": "dbguard", "sha256": DIGEST},
                {"name": "core", "path": "other", "sha256": OTHER},
            ],
            ERROR_COMPONENT_PATH_INVALID: [
                {"name": "core", "path": "../escape", "sha256": DIGEST},
            ],
            ERROR_COMPONENT_DIGEST_INVALID: [
                {"name": "core", "path": "dbguard", "sha256": "not-a-digest"},
            ],
        }
        for expected, components in cases.items():
            with self.subTest(expected=expected):
                _, error = validate_components(document(components=components))
                self.assertEqual(error, expected)

    def test_uppercase_digest_is_normalized_not_rejected(self):
        components, error = validate_components(
            document(
                components=[
                    {"name": "core", "path": "dbguard", "sha256": DIGEST.upper()},
                    {"name": "agent", "path": "agent-app", "sha256": OTHER},
                ]
            )
        )
        self.assertIsNone(error)
        self.assertEqual(components[0].sha256, DIGEST)


if __name__ == "__main__":
    unittest.main()
